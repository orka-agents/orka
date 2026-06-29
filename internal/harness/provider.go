package harness

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"strings"
)

const localhostName = "localhost"

// KubernetesServiceProvider is the first boring, non-Substrate harness provider.
// It resolves an already-routable Kubernetes Service/base URL to the provider-
// neutral HTTP+SSE harness protocol endpoint.
type KubernetesServiceProvider struct {
	EndpointURL           string
	AllowInsecureLoopback bool
}

func (p KubernetesServiceProvider) Endpoint(_ context.Context, owner RuntimeSessionOwner) (string, error) {
	endpoint := strings.TrimSpace(p.EndpointURL)
	if endpoint == "" {
		return "", fmt.Errorf("harness endpoint is required")
	}
	if strings.TrimSpace(owner.Namespace) == "" {
		return "", fmt.Errorf("namespace is required")
	}
	if strings.TrimSpace(owner.SessionName) == "" {
		return "", fmt.Errorf("session name is required")
	}
	if strings.TrimSpace(string(owner.Provider)) == "" {
		return "", fmt.Errorf("runtime provider is required")
	}
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return "", fmt.Errorf("parse harness endpoint: %w", err)
	}
	if parsed.Scheme == "" || parsed.Host == "" {
		return "", fmt.Errorf("harness endpoint must include scheme and host")
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", fmt.Errorf("harness endpoint scheme must be http or https")
	}
	if parsed.User != nil {
		return "", fmt.Errorf("harness endpoint must not include user info")
	}
	if err := p.validateServiceHost(parsed, owner.Namespace); err != nil {
		return "", err
	}
	return parsed.String(), nil
}

func (p KubernetesServiceProvider) validateServiceHost(parsed *url.URL, namespace string) error {
	host := parsed.Hostname()
	if host == "" {
		return fmt.Errorf("harness endpoint host is required")
	}
	if ip := net.ParseIP(host); ip != nil {
		if p.AllowInsecureLoopback && ip.IsLoopback() {
			return nil
		}
		return fmt.Errorf("harness endpoint must be a Kubernetes Service DNS name, not an IP address")
	}
	normalizedHost := strings.TrimSuffix(strings.ToLower(host), ".")
	if p.AllowInsecureLoopback && (normalizedHost == localhostName || strings.HasSuffix(normalizedHost, "."+localhostName)) {
		return nil
	}
	namespace = strings.ToLower(strings.TrimSpace(namespace))
	if namespace == "" {
		return fmt.Errorf("namespace is required")
	}
	if strings.HasSuffix(normalizedHost, "."+namespace+".svc") ||
		strings.HasSuffix(normalizedHost, "."+namespace+".svc.cluster.local") {
		return nil
	}
	return fmt.Errorf("harness endpoint host must be a Service DNS name in namespace %q", namespace)
}
