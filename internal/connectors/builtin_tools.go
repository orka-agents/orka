/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package connectors

import (
	"net/url"
	"slices"
	"strings"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
)

// BuiltinConnectorToolAudience is the resource server every catalog
// built-in sends its credential to. It is fixed in the tool
// implementations, so only a provider issuing credentials for it may
// declare them; see ProviderIssuesGitHubCredentials.
const BuiltinConnectorToolAudience = "https://api.github.com"

// builtinConnectorIssuerHost is the OAuth host whose tokens the catalog
// built-ins' audience accepts. GitHub Enterprise Server issues tokens for
// its own API host, which the built-ins never call.
const builtinConnectorIssuerHost = "github.com"

// ProviderIssuesGitHubCredentials reports whether provider's OAuth
// endpoints are github.com's, so a token it issues is meant for
// BuiltinConnectorToolAudience and nothing else.
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

// builtinConnectorTools are the Orka built-in tools that consume a person's
// linked-account credential when a ConnectorProvider declares them with
// source Builtin. Their class is fixed by what the tool does with the
// credential: read-class tools only read through it, write-class tools
// mutate the forge with it. A provider cannot declare a different class,
// and a built-in outside this catalog cannot be declared at all, because it
// would ignore the credential and silently run under something else.
var builtinConnectorTools = map[string]corev1alpha1.ConnectorToolClass{
	"check_pr_review_marker": corev1alpha1.ConnectorToolClassRead,
	"check_pull_request_ci":  corev1alpha1.ConnectorToolClassRead,
	"get_issue":              corev1alpha1.ConnectorToolClassRead,
	"list_issues":            corev1alpha1.ConnectorToolClassRead,
	"list_pull_requests":     corev1alpha1.ConnectorToolClassRead,
	"review_pull_request":    corev1alpha1.ConnectorToolClassRead,
	"comment_on_issue":       corev1alpha1.ConnectorToolClassWrite,
	"create_pull_request":    corev1alpha1.ConnectorToolClassWrite,
	"post_review_comment":    corev1alpha1.ConnectorToolClassWrite,
}

// BuiltinConnectorToolClass returns the fixed class of a built-in tool that
// can run under a linked account, and false for every other name.
func BuiltinConnectorToolClass(name string) (corev1alpha1.ConnectorToolClass, bool) {
	class, ok := builtinConnectorTools[name]
	return class, ok
}

// BuiltinConnectorToolNames returns the catalog names in sorted order.
func BuiltinConnectorToolNames() []string {
	names := make([]string, 0, len(builtinConnectorTools))
	for name := range builtinConnectorTools {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

// DeclaresBuiltinTool reports whether provider declares name as a Builtin
// tool, and returns the declaration.
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
