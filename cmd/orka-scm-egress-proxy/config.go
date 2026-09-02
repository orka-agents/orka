/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"sort"
	"strings"
	"time"
)

const (
	defaultListenAddress         = ":8080"
	defaultAllowedHosts          = "github.com"
	defaultForgeAPIBaseURL       = "https://api.github.com"
	defaultTokenFile             = "/var/run/secrets/orka/scm-egress/token"
	defaultKubernetesTokenFile   = "/var/run/secrets/kubernetes.io/serviceaccount/token"
	defaultMaxRequestHeaderBytes = int64(32 << 10)
	defaultMaxResponseHeader     = int64(64 << 10)
	defaultMaxRequestBytes       = int64(4 << 20)
	defaultMaxResponseBytes      = int64(8 << 20)
	defaultMaxTunnelBytes        = int64(1 << 30)
	defaultMaxConcurrent         = 8
	defaultResolutionTimeout     = 5 * time.Second
	defaultConnectTimeout        = 10 * time.Second
	defaultResponseHeaderTimeout = 30 * time.Second
	defaultForwardTimeout        = 2 * time.Minute
	defaultIdleTimeout           = 30 * time.Second
	defaultTunnelTimeout         = 10 * time.Minute
	defaultShutdownTimeout       = 15 * time.Second
	proxyUsername                = "orka-publisher"
	maxProxyTokenBytes           = 256
	minProxyTokenBytes           = 32
	maxResolvedAddresses         = 32
)

var (
	errHostDenied       = errors.New("target host is not allowed")
	errAddressDenied    = errors.New("target address is not public")
	errResolutionFailed = errors.New("target resolution failed")
)

type resolver interface {
	LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error)
}

type contextDialer interface {
	DialContext(ctx context.Context, network, address string) (net.Conn, error)
}

type proxyConfig struct {
	AllowedHosts          map[string]struct{}
	MaxRequestHeaderBytes int64
	MaxResponseHeader     int64
	MaxRequestBytes       int64
	MaxResponseBytes      int64
	MaxTunnelBytes        int64
	MaxConcurrent         int
	ResolutionTimeout     time.Duration
	ConnectTimeout        time.Duration
	ResponseHeaderTimeout time.Duration
	ForwardTimeout        time.Duration
	IdleTimeout           time.Duration
	TunnelTimeout         time.Duration
	Resolver              resolver
	Dialer                contextDialer
}

func normalizeProxyConfig(config proxyConfig) (proxyConfig, error) {
	if len(config.AllowedHosts) == 0 {
		return proxyConfig{}, fmt.Errorf("at least one allowed host is required")
	}
	for host := range config.AllowedHosts {
		if err := validateHostname(host); err != nil {
			return proxyConfig{}, fmt.Errorf("allowed host is invalid: %w", err)
		}
	}
	applyProxyConfigDefaults(&config)
	if err := validateProxyConfigBounds(config); err != nil {
		return proxyConfig{}, err
	}
	if config.Resolver == nil {
		config.Resolver = net.DefaultResolver
	}
	if config.Dialer == nil {
		config.Dialer = &net.Dialer{Timeout: config.ConnectTimeout, KeepAlive: -1}
	}
	return config, nil
}

func applyProxyConfigDefaults(config *proxyConfig) {
	if config.MaxRequestHeaderBytes == 0 {
		config.MaxRequestHeaderBytes = defaultMaxRequestHeaderBytes
	}
	if config.MaxResponseHeader == 0 {
		config.MaxResponseHeader = defaultMaxResponseHeader
	}
	if config.MaxRequestBytes == 0 {
		config.MaxRequestBytes = defaultMaxRequestBytes
	}
	if config.MaxResponseBytes == 0 {
		config.MaxResponseBytes = defaultMaxResponseBytes
	}
	if config.MaxTunnelBytes == 0 {
		config.MaxTunnelBytes = defaultMaxTunnelBytes
	}
	if config.MaxConcurrent == 0 {
		config.MaxConcurrent = defaultMaxConcurrent
	}
	if config.ResolutionTimeout == 0 {
		config.ResolutionTimeout = defaultResolutionTimeout
	}
	if config.ConnectTimeout == 0 {
		config.ConnectTimeout = defaultConnectTimeout
	}
	if config.ResponseHeaderTimeout == 0 {
		config.ResponseHeaderTimeout = defaultResponseHeaderTimeout
	}
	if config.ForwardTimeout == 0 {
		config.ForwardTimeout = defaultForwardTimeout
	}
	if config.IdleTimeout == 0 {
		config.IdleTimeout = defaultIdleTimeout
	}
	if config.TunnelTimeout == 0 {
		config.TunnelTimeout = defaultTunnelTimeout
	}
}

func validateProxyConfigBounds(config proxyConfig) error {
	if config.MaxRequestHeaderBytes < 1024 || config.MaxRequestHeaderBytes > 1<<20 ||
		config.MaxResponseHeader < 1024 || config.MaxResponseHeader > 1<<20 ||
		config.MaxRequestBytes < 1 || config.MaxRequestBytes > 1<<30 ||
		config.MaxResponseBytes < 1 || config.MaxResponseBytes > 1<<30 ||
		config.MaxTunnelBytes < 1 || config.MaxTunnelBytes > 16<<30 ||
		config.MaxConcurrent < 1 || config.MaxConcurrent > 1024 ||
		config.ResolutionTimeout <= 0 || config.ResolutionTimeout > time.Minute ||
		config.ConnectTimeout <= 0 || config.ConnectTimeout > time.Minute ||
		config.ResponseHeaderTimeout <= 0 || config.ResponseHeaderTimeout > 5*time.Minute ||
		config.ForwardTimeout <= 0 || config.ForwardTimeout > 30*time.Minute ||
		config.IdleTimeout <= 0 || config.IdleTimeout > 5*time.Minute ||
		config.TunnelTimeout <= 0 || config.TunnelTimeout > time.Hour {
		return fmt.Errorf("proxy limits are invalid")
	}
	return nil
}

func allowedHosts(rawHosts, rawForgeAPI string) (map[string]struct{}, error) {
	hosts := make(map[string]struct{})
	for raw := range strings.SplitSeq(rawHosts, ",") {
		host := strings.TrimSpace(raw)
		if host == "" {
			continue
		}
		if err := validateHostname(host); err != nil {
			return nil, fmt.Errorf("allowed host is invalid: %w", err)
		}
		hosts[host] = struct{}{}
	}
	if strings.TrimSpace(rawForgeAPI) != "" {
		forgeHost, err := forgeAPIHostname(rawForgeAPI)
		if err != nil {
			return nil, err
		}
		hosts[forgeHost] = struct{}{}
	}
	if len(hosts) == 0 {
		return nil, fmt.Errorf("at least one allowed host is required")
	}
	return hosts, nil
}

func forgeAPIHostname(raw string) (string, error) {
	if strings.TrimSpace(raw) != raw || len(raw) > 2048 {
		return "", fmt.Errorf("forge API base URL is invalid")
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil ||
		parsed.RawQuery != "" || parsed.Fragment != "" || parsed.Opaque != "" {
		return "", fmt.Errorf("forge API base URL is invalid")
	}
	if parsed.Port() != "" && parsed.Port() != "443" {
		return "", fmt.Errorf("forge API base URL must use port 443")
	}
	host := parsed.Hostname()
	if err := validateHostname(host); err != nil {
		return "", fmt.Errorf("forge API base URL host is invalid: %w", err)
	}
	return host, nil
}

func validateHostname(host string) error {
	if host == "" || len(host) > 253 || host != strings.ToLower(host) || strings.HasSuffix(host, ".") ||
		net.ParseIP(host) != nil {
		return fmt.Errorf("hostname must be an exact lower-case DNS name")
	}
	labels := strings.Split(host, ".")
	if len(labels) < 2 {
		return fmt.Errorf("hostname must contain at least two labels")
	}
	for _, label := range labels {
		if err := validateHostnameLabel(label); err != nil {
			return err
		}
	}
	return nil
}

func validateHostnameLabel(label string) error {
	if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
		return fmt.Errorf("hostname label is invalid")
	}
	for _, current := range label {
		if (current < 'a' || current > 'z') && (current < '0' || current > '9') && current != '-' {
			return fmt.Errorf("hostname label contains an unsupported character")
		}
	}
	return nil
}

func sortedAddresses(addresses []netip.Addr) []netip.Addr {
	result := append([]netip.Addr(nil), addresses...)
	sort.Slice(result, func(left, right int) bool {
		return result[left].Compare(result[right]) < 0
	})
	return result
}
