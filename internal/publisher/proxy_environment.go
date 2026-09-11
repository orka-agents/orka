package publisher

import (
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
)

const proxyUsername = "orka-publisher"

// ProxyEnvironment is an explicitly reviewed HTTPS proxy configuration for
// trusted Git subprocesses. Ambient proxy variables remain ignored unless the
// publisher command opts into this configuration during startup.
type ProxyEnvironment struct {
	HTTPSProxy string
	NoProxy    string
}

// NormalizeProxyEnvironment validates the authenticated in-cluster forward
// proxy and the narrow bypass list before either value enters a Git child.
func NormalizeProxyEnvironment(rawHTTPSProxy, rawNoProxy string) (ProxyEnvironment, error) {
	if rawHTTPSProxy == "" && rawNoProxy == "" {
		return ProxyEnvironment{}, nil
	}
	if strings.TrimSpace(rawHTTPSProxy) != rawHTTPSProxy || strings.TrimSpace(rawNoProxy) != rawNoProxy ||
		rawHTTPSProxy == "" || rawNoProxy == "" {
		return ProxyEnvironment{}, fmt.Errorf("publisher proxy environment is incomplete or non-canonical")
	}
	if err := validateHTTPSProxyURL(rawHTTPSProxy); err != nil {
		return ProxyEnvironment{}, err
	}
	if err := validateNoProxy(rawNoProxy); err != nil {
		return ProxyEnvironment{}, err
	}
	return ProxyEnvironment{HTTPSProxy: rawHTTPSProxy, NoProxy: rawNoProxy}, nil
}

func validateHTTPSProxyURL(raw string) error {
	if len(raw) > 1024 {
		return fmt.Errorf("publisher HTTPS proxy URL is invalid")
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme != "http" || parsed.Host == "" || parsed.User == nil ||
		(parsed.Path != "" && parsed.Path != "/") || parsed.RawQuery != "" || parsed.Fragment != "" ||
		parsed.Opaque != "" {
		return fmt.Errorf("publisher HTTPS proxy URL is invalid")
	}
	password, hasPassword := parsed.User.Password()
	if parsed.User.Username() != proxyUsername || !hasPassword || len(password) < 32 || len(password) > 256 ||
		!validProxyPassword(password) {
		return fmt.Errorf("publisher HTTPS proxy authentication is invalid")
	}
	if parsed.Hostname() != strings.ToLower(parsed.Hostname()) || net.ParseIP(parsed.Hostname()) != nil ||
		!validProxyHostname(parsed.Hostname()) || !strings.HasSuffix(parsed.Hostname(), ".svc") {
		return fmt.Errorf("publisher HTTPS proxy host is invalid")
	}
	port, err := strconv.Atoi(parsed.Port())
	if err != nil || port < 1 || port > 65535 {
		return fmt.Errorf("publisher HTTPS proxy port is invalid")
	}
	return nil
}

func validProxyPassword(password string) bool {
	for _, current := range password {
		if (current < 'a' || current > 'z') && (current < 'A' || current > 'Z') &&
			(current < '0' || current > '9') && !strings.ContainsRune("-._~", current) {
			return false
		}
	}
	return true
}

func validProxyHostname(host string) bool {
	if host == "" || len(host) > 253 || strings.HasSuffix(host, ".") {
		return false
	}
	for label := range strings.SplitSeq(host, ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, current := range label {
			if (current < 'a' || current > 'z') && (current < '0' || current > '9') && current != '-' {
				return false
			}
		}
	}
	return true
}

func validateNoProxy(raw string) error {
	if len(raw) > 2048 {
		return fmt.Errorf("publisher NO_PROXY is invalid")
	}
	seen := make(map[string]struct{})
	for token := range strings.SplitSeq(raw, ",") {
		if token == "" || strings.TrimSpace(token) != token || !safeNoProxyToken(token) {
			return fmt.Errorf("publisher NO_PROXY contains an unsafe bypass")
		}
		if _, exists := seen[token]; exists {
			return fmt.Errorf("publisher NO_PROXY contains a duplicate bypass")
		}
		seen[token] = struct{}{}
	}
	if len(seen) == 0 {
		return fmt.Errorf("publisher NO_PROXY is empty")
	}
	return nil
}

func safeNoProxyToken(token string) bool {
	switch token {
	case "localhost", "127.0.0.1", "::1", ".svc", ".cluster.local":
		return true
	default:
		return strings.HasSuffix(token, ".svc") || strings.HasSuffix(token, ".svc.cluster.local")
	}
}

// Variables returns the only proxy variables allowed into Git subprocesses.
func (environment ProxyEnvironment) Variables() []string {
	if environment.HTTPSProxy == "" {
		return nil
	}
	return []string{
		"HTTPS_PROXY=" + environment.HTTPSProxy,
		"https_proxy=" + environment.HTTPSProxy,
		"NO_PROXY=" + environment.NoProxy,
		"no_proxy=" + environment.NoProxy,
	}
}
