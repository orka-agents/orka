/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package gateway

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	gatewayv1alpha1 "github.com/orka-agents/orka/api/gateway/v1alpha1"
)

const (
	GatewayInboundAuthLabel       = "gateway.orka.ai/inbound-auth"
	GatewayOutboundAuthLabel      = "gateway.orka.ai/outbound-auth"
	GatewayAuthNameLabel          = "gateway.orka.ai/gateway-name"
	GatewayAuthNameAnnotation     = "gateway.orka.ai/gateway-name"
	GatewayAuthEndpointAnnotation = "gateway.orka.ai/adapter-endpoint"
	GatewayAuthEnabledValue       = "true"
)

// EndpointResolver applies the gateway endpoint SSRF policy.
type EndpointResolver struct {
	Client                client.Reader
	AllowInsecureLoopback bool
}

// Resolve returns the adapter base URL and an operator-safe scheme/host form.
func (r EndpointResolver) Resolve(ctx context.Context, object *gatewayv1alpha1.Gateway) (string, string, error) {
	if object == nil {
		return "", "", fmt.Errorf("gateway is required")
	}
	location := object.Spec.Adapter
	if strings.TrimSpace(location.Endpoint) != "" && location.ServiceRef != nil {
		return "", "", fmt.Errorf("exactly one of adapter.endpoint or adapter.serviceRef is required")
	}
	if endpoint := strings.TrimSpace(location.Endpoint); endpoint != "" {
		parsed, err := validateAbsoluteEndpoint(endpoint)
		if err != nil {
			return "", "", err
		}
		loopbackAllowed := r.AllowInsecureLoopback && isLoopbackHost(parsed.Hostname())
		if parsed.Scheme == "http" && !loopbackAllowed {
			return "", "", fmt.Errorf("adapter.endpoint must use https")
		}
		if !loopbackAllowed {
			if err := validateDirectEndpointHost(parsed.Hostname()); err != nil {
				return "", "", err
			}
		}
		return strings.TrimRight(parsed.String(), "/"), sanitizeEndpoint(parsed), nil
	}
	if location.ServiceRef == nil || strings.TrimSpace(location.ServiceRef.Name) == "" {
		return "", "", fmt.Errorf("exactly one of adapter.endpoint or adapter.serviceRef is required")
	}
	if r.Client == nil {
		return "", "", fmt.Errorf("kubernetes client is required to resolve adapter.serviceRef")
	}
	service := &corev1.Service{}
	key := client.ObjectKey{Namespace: object.Namespace, Name: strings.TrimSpace(location.ServiceRef.Name)}
	if err := r.Client.Get(ctx, key, service); err != nil {
		if apierrors.IsNotFound(err) {
			return "", "", fmt.Errorf("adapter Service %q not found", key.Name)
		}
		return "", "", fmt.Errorf("read adapter Service %q: %w", key.Name, err)
	}
	if service.Spec.Type == corev1.ServiceTypeExternalName || len(service.Spec.Selector) == 0 {
		return "", "", fmt.Errorf("adapter Service %q must be selector-backed and must not be ExternalName", key.Name)
	}
	port := location.ServiceRef.Port
	if port == 0 {
		if len(service.Spec.Ports) == 0 {
			return "", "", fmt.Errorf("adapter Service %q has no ports", key.Name)
		}
		port = service.Spec.Ports[0].Port
	} else {
		found := false
		for _, declared := range service.Spec.Ports {
			if declared.Port == port {
				found = true
				break
			}
		}
		if !found {
			return "", "", fmt.Errorf("adapter Service %q does not expose port %d", key.Name, port)
		}
	}
	// Service selectors constrain routing, but TLS authentication of the Service DNS name is
	// what prevents a relabeled backend from receiving the outbound bearer token.
	host := key.Name + "." + object.Namespace + ".svc"
	endpoint := "https://" + host + ":" + strconv.Itoa(int(port))
	return endpoint, endpoint, nil
}

func validateAbsoluteEndpoint(endpoint string) (*url.URL, error) {
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return nil, fmt.Errorf("adapter endpoint must be an absolute http(s) URL")
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, fmt.Errorf("adapter endpoint scheme %q is not supported", parsed.Scheme)
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, fmt.Errorf("adapter endpoint must not contain credentials, query, or fragment components")
	}
	return parsed, nil
}

func sanitizeEndpoint(parsed *url.URL) string {
	if parsed == nil {
		return ""
	}
	return parsed.Scheme + "://" + parsed.Host
}

func isLoopbackHost(host string) bool {
	host = strings.Trim(strings.ToLower(strings.TrimSpace(host)), "[]")
	if host == "localhost" {
		return true
	}
	address, err := netip.ParseAddr(host)
	return err == nil && address.IsLoopback()
}

var restrictedGatewayAddressPrefixes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("10.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("127.0.0.0/8"),
	netip.MustParsePrefix("169.254.0.0/16"),
	netip.MustParsePrefix("172.16.0.0/12"),
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("192.168.0.0/16"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("224.0.0.0/4"),
	netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("::/128"),
	netip.MustParsePrefix("::1/128"),
	netip.MustParsePrefix("64:ff9b::/96"),
	netip.MustParsePrefix("64:ff9b:1::/48"),
	netip.MustParsePrefix("100::/64"),
	netip.MustParsePrefix("2001::/23"),
	netip.MustParsePrefix("2002::/16"),
	netip.MustParsePrefix("3fff::/20"),
	netip.MustParsePrefix("5f00::/16"),
	netip.MustParsePrefix("fc00::/7"),
	netip.MustParsePrefix("fe80::/10"),
	netip.MustParsePrefix("fec0::/10"),
	netip.MustParsePrefix("ff00::/8"),
	netip.MustParsePrefix("2001:db8::/32"),
}

func validateDirectEndpointHost(host string) error {
	host = strings.Trim(strings.ToLower(strings.TrimSpace(host)), "[]")
	if host == "" {
		return fmt.Errorf("adapter endpoint host is required")
	}
	if host == "localhost" || strings.HasSuffix(host, ".localhost") || strings.HasSuffix(host, ".local") ||
		host == "svc" || strings.HasSuffix(host, ".svc") || strings.Contains(host, ".svc.") {
		return fmt.Errorf("adapter.endpoint must not target local or Kubernetes Service names")
	}
	if address, err := netip.ParseAddr(host); err == nil && !isPublicGatewayAddress(address) {
		return fmt.Errorf("adapter.endpoint must not target a non-public address")
	}
	return nil
}

func isPublicGatewayAddress(address netip.Addr) bool {
	if !address.IsValid() {
		return false
	}
	address = address.Unmap()
	if !address.IsGlobalUnicast() || address.IsPrivate() || address.IsLoopback() ||
		address.IsLinkLocalUnicast() || address.IsLinkLocalMulticast() || address.IsMulticast() || address.IsUnspecified() {
		return false
	}
	for _, prefix := range restrictedGatewayAddressPrefixes {
		if prefix.Contains(address) {
			return false
		}
	}
	return true
}

func publicGatewayDialContext(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, fmt.Errorf("parse adapter dial address: %w", err)
	}
	var addresses []netip.Addr
	if literal, parseErr := netip.ParseAddr(strings.Trim(host, "[]")); parseErr == nil {
		addresses = []netip.Addr{literal}
	} else {
		addresses, err = net.DefaultResolver.LookupNetIP(ctx, "ip", host)
		if err != nil {
			return nil, fmt.Errorf("resolve adapter endpoint: %w", err)
		}
	}
	if len(addresses) == 0 {
		return nil, fmt.Errorf("adapter endpoint resolved to no addresses")
	}
	for _, resolved := range addresses {
		if !isPublicGatewayAddress(resolved) {
			return nil, fmt.Errorf("adapter endpoint resolved to a non-public address")
		}
	}
	dialer := &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}
	var lastErr error
	for _, resolved := range addresses {
		connection, dialErr := dialer.DialContext(ctx, network, net.JoinHostPort(resolved.String(), port))
		if dialErr == nil {
			return connection, nil
		}
		lastErr = dialErr
	}
	return nil, fmt.Errorf("dial adapter endpoint: %w", lastErr)
}
