/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package connectors

import (
	"net/url"
	"slices"
	"strings"
	"time"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
)

const BuiltinConnectorToolAudience = "https://api.github.com"

const builtinConnectorIssuerHost = "github.com"

func ProviderIssuesGitHubCredentials(provider *corev1alpha1.ConnectorProvider) bool {
	if provider == nil {
		return false
	}
	for _, raw := range []string{provider.Spec.OAuth.AuthorizeURL, provider.Spec.OAuth.TokenURL} {
		parsed, err := url.Parse(strings.TrimSpace(raw))
		if err != nil || parsed.Scheme != "https" || !strings.EqualFold(parsed.Hostname(), builtinConnectorIssuerHost) ||
			(parsed.Port() != "" && parsed.Port() != "443") {
			return false
		}
	}
	return true
}

var builtinConnectorTools = map[string]builtinConnectorTool{
	"check_pull_request_ci": {Class: corev1alpha1.ConnectorToolClassRead, Timeout: BuiltinPollingToolTimeout},
	"get_issue":             {Class: corev1alpha1.ConnectorToolClassRead, Timeout: BuiltinToolTimeout},
	"list_issues":           {Class: corev1alpha1.ConnectorToolClassRead, Timeout: BuiltinToolTimeout},
	"list_pull_requests":    {Class: corev1alpha1.ConnectorToolClassRead, Timeout: BuiltinToolTimeout},
	"review_pull_request":   {Class: corev1alpha1.ConnectorToolClassRead, Timeout: BuiltinToolTimeout},
	"comment_on_issue":      {Class: corev1alpha1.ConnectorToolClassWrite, Timeout: BuiltinToolTimeout},
	"create_pull_request":   {Class: corev1alpha1.ConnectorToolClassWrite, Timeout: BuiltinToolTimeout},
	"post_review_comment":   {Class: corev1alpha1.ConnectorToolClassWrite, Timeout: BuiltinToolTimeout},
}

type builtinConnectorTool struct {
	Class   corev1alpha1.ConnectorToolClass
	Timeout time.Duration
}

const (
	BuiltinToolTimeout        = 2 * time.Minute
	BuiltinPollingToolTimeout = 11 * time.Minute
)

func BuiltinConnectorToolClass(name string) (corev1alpha1.ConnectorToolClass, bool) {
	tool, ok := builtinConnectorTools[name]
	return tool.Class, ok
}

func BuiltinConnectorToolTimeout(name string) (time.Duration, bool) {
	tool, ok := builtinConnectorTools[name]
	return tool.Timeout, ok
}

func BuiltinConnectorToolNames() []string {
	names := make([]string, 0, len(builtinConnectorTools))
	for name := range builtinConnectorTools {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

func DeclaresBuiltinTool(provider *corev1alpha1.ConnectorProvider, name string) (corev1alpha1.ConnectorTool, bool) {
	if provider == nil {
		return corev1alpha1.ConnectorTool{}, false
	}
	for _, tool := range provider.Spec.Tools {
		if tool.Name == name && tool.Source == corev1alpha1.ConnectorToolSourceBuiltin {
			return tool, true
		}
	}
	return corev1alpha1.ConnectorTool{}, false
}
