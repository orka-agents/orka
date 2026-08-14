/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

// Package endpointpolicy provides the fail-closed network policy used for
// credentials-bearing calls to public HTTPS control-plane endpoints.
package endpointpolicy

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"

	"golang.org/x/net/idna"
)

const (
	maxEndpointLength        = 2048
	defaultHTTPSPort         = "443"
	defaultPublicDialTimeout = 30 * time.Second
)

// IPResolver resolves all IP addresses for one host.
type IPResolver interface {
	LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error)
}

// ContextDialer opens a network connection.
type ContextDialer interface {
	DialContext(ctx context.Context, network, address string) (net.Conn, error)
}

// PublicHTTPSPolicy enforces public-only HTTPS resolution and dialing. Resolver
// and Dialer are injectable for deterministic tests; nil uses the Go defaults.
type PublicHTTPSPolicy struct {
	Resolver IPResolver
	Dialer   ContextDialer
}

// Resolution is a canonical public endpoint plus its reconciliation-time DNS identity.
type Resolution struct {
	BaseURL               string
	Identity              string
	EndpointDigest        string
	ResolvedAddressDigest string
	Addresses             []netip.Addr
}

// Resolve canonicalizes a public HTTPS endpoint and validates every current A
// and AAAA answer. A single restricted answer rejects the entire endpoint.
func (p PublicHTTPSPolicy) Resolve(ctx context.Context, raw string) (Resolution, error) {
	identity, host, err := canonicalPublicHTTPSEndpoint(raw)
	if err != nil {
		return Resolution{}, err
	}
	addresses, err := p.resolveHost(ctx, host)
	if err != nil {
		return Resolution{}, err
	}
	return Resolution{
		BaseURL:               identity,
		Identity:              identity,
		EndpointDigest:        digestString(identity),
		ResolvedAddressDigest: addressDigest(addresses),
		Addresses:             append([]netip.Addr(nil), addresses...),
	}, nil
}

// NewHTTPClient returns an HTTP client whose transport disables proxies,
// validates DNS answers on every dial, preserves hostname/SNI verification,
// rejects redirects, disables transparent compression, and bounds timeouts.
// Callers that already resolved an endpoint for an identity decision should use
// NewPinnedHTTPClient so the credential-bearing request cannot observe a second
// DNS answer.
func (p PublicHTTPSPolicy) NewHTTPClient(base *http.Client, timeout time.Duration) (*http.Client, error) {
	return p.newHTTPClient(base, timeout, p.DialContext)
}

// NewPinnedHTTPClient returns an HTTP client that dials only the exact public
// address set represented by resolution. The request URL keeps its canonical
// hostname, so TLS SNI and certificate verification continue to use that host,
// while the dial path performs no second resolver lookup.
func (p PublicHTTPSPolicy) NewPinnedHTTPClient(
	base *http.Client,
	timeout time.Duration,
	resolution Resolution,
) (*http.Client, error) {
	host, addresses, err := validatePinnedResolution(resolution)
	if err != nil {
		return nil, err
	}
	return p.newHTTPClient(base, timeout, p.pinnedDialContext(host, addresses))
}

func (p PublicHTTPSPolicy) newHTTPClient(
	base *http.Client,
	timeout time.Duration,
	dialContext func(context.Context, string, string) (net.Conn, error),
) (*http.Client, error) {
	if timeout <= 0 {
		return nil, fmt.Errorf("public HTTPS client timeout must be positive")
	}
	if dialContext == nil {
		return nil, fmt.Errorf("public HTTPS client dial policy is required")
	}
	result := &http.Client{Timeout: timeout}
	if base != nil {
		copy := *base
		result = &copy
		result.Timeout = timeout
	}

	var transport *http.Transport
	switch current := result.Transport.(type) {
	case nil:
		defaultTransport, ok := http.DefaultTransport.(*http.Transport)
		if !ok {
			return nil, fmt.Errorf("default HTTP transport is not configurable")
		}
		transport = defaultTransport.Clone()
	case *http.Transport:
		transport = current.Clone()
	default:
		return nil, fmt.Errorf("custom HTTP transport cannot enforce public endpoint policy")
	}

	if transport.TLSClientConfig != nil {
		if transport.TLSClientConfig.InsecureSkipVerify { //nolint:gosec // explicitly rejected
			return nil, fmt.Errorf("public HTTPS client must verify server certificates")
		}
		if strings.TrimSpace(transport.TLSClientConfig.ServerName) != "" {
			return nil, fmt.Errorf("public HTTPS client must derive TLS server identity from the request host")
		}
		transport.TLSClientConfig = transport.TLSClientConfig.Clone()
	} else {
		transport.TLSClientConfig = &tls.Config{}
	}
	if transport.TLSClientConfig.MinVersion < tls.VersionTLS12 {
		transport.TLSClientConfig.MinVersion = tls.VersionTLS12
	}

	// A custom TLS dial hook can bypass the validated public-only DialContext.
	//nolint:staticcheck // DialTLS is deprecated but must still be rejected when supplied by a cloned transport.
	if transport.DialTLS != nil || transport.DialTLSContext != nil {
		return nil, fmt.Errorf("custom TLS dial hooks cannot enforce public endpoint policy")
	}
	transport.Proxy = nil
	transport.DialContext = dialContext
	transport.DisableCompression = true
	// Callers construct endpoint-bound clients after a fresh identity check.
	// Disabling keep-alives prevents each short-lived client from retaining an
	// otherwise unreachable idle transport pool after the operation completes.
	transport.DisableKeepAlives = true
	transport.ForceAttemptHTTP2 = true
	transport.MaxIdleConns = 0
	transport.MaxIdleConnsPerHost = -1
	transport.MaxConnsPerHost = 4
	transport.IdleConnTimeout = minDuration(transport.IdleConnTimeout, timeout)
	transport.TLSHandshakeTimeout = minPositiveDuration(transport.TLSHandshakeTimeout, timeout)
	transport.ResponseHeaderTimeout = minPositiveDuration(transport.ResponseHeaderTimeout, timeout)
	transport.ExpectContinueTimeout = minPositiveDuration(transport.ExpectContinueTimeout, timeout)

	result.Transport = transport
	result.Jar = nil
	result.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return result, nil
}

// DialContext resolves and validates every address immediately before dialing.
// It never delegates DNS resolution to the underlying dialer.
func (p PublicHTTPSPolicy) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, fmt.Errorf("parse public HTTPS dial address: %w", err)
	}
	host = strings.Trim(host, "[]")
	if err := validatePublicHostName(host); err != nil {
		return nil, err
	}
	addresses, err := p.resolveHost(ctx, host)
	if err != nil {
		return nil, err
	}
	return p.dialAddresses(ctx, network, port, addresses)
}

func (p PublicHTTPSPolicy) pinnedDialContext(
	expectedHost string,
	addresses []netip.Addr,
) func(context.Context, string, string) (net.Conn, error) {
	pinned := append([]netip.Addr(nil), addresses...)
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, fmt.Errorf("parse pinned public HTTPS dial address: %w", err)
		}
		host, err = canonicalDialHost(host)
		if err != nil {
			return nil, err
		}
		if host != expectedHost {
			return nil, fmt.Errorf("pinned public HTTPS client rejected a different request host")
		}
		return p.dialAddresses(ctx, network, port, pinned)
	}
}

func (p PublicHTTPSPolicy) dialAddresses(
	ctx context.Context,
	network, port string,
	addresses []netip.Addr,
) (net.Conn, error) {
	dialer := p.Dialer
	if dialer == nil {
		dialer = &net.Dialer{Timeout: defaultPublicDialTimeout, KeepAlive: 30 * time.Second}
	}
	compatible := make([]netip.Addr, 0, len(addresses))
	for _, resolved := range addresses {
		if network == "tcp4" && !resolved.Is4() {
			continue
		}
		if network == "tcp6" && !resolved.Is6() {
			continue
		}
		compatible = append(compatible, resolved)
	}
	if len(compatible) == 0 {
		return nil, fmt.Errorf("public HTTPS endpoint has no address compatible with the requested network")
	}

	overallCtx := ctx
	if _, hasDeadline := overallCtx.Deadline(); !hasDeadline {
		var cancel context.CancelFunc
		overallCtx, cancel = context.WithTimeout(overallCtx, defaultPublicDialTimeout)
		defer cancel()
	}
	var lastErr error
	for index, resolved := range compatible {
		deadline, ok := overallCtx.Deadline()
		if !ok {
			return nil, fmt.Errorf("public HTTPS dial context has no deadline")
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			lastErr = overallCtx.Err()
			break
		}
		// Divide the remaining overall budget among this and all later addresses.
		// A stalled first address therefore cannot consume the entire request and
		// every validated address receives a bounded connection attempt.
		attemptBudget := remaining / time.Duration(len(compatible)-index)
		if attemptBudget <= 0 {
			attemptBudget = remaining
		}
		attemptCtx, cancel := context.WithTimeout(overallCtx, attemptBudget)
		connection, dialErr := dialer.DialContext(attemptCtx, network, net.JoinHostPort(resolved.String(), port))
		cancel()
		if dialErr == nil {
			return connection, nil
		}
		lastErr = dialErr
	}
	if lastErr == nil {
		lastErr = overallCtx.Err()
	}
	return nil, fmt.Errorf("dial public HTTPS endpoint: %w", lastErr)
}

func validatePinnedResolution(resolution Resolution) (string, []netip.Addr, error) {
	identity, host, err := canonicalPublicHTTPSEndpoint(resolution.Identity)
	if err != nil || resolution.BaseURL != identity || resolution.Identity != identity {
		return "", nil, fmt.Errorf("pinned public HTTPS resolution identity is invalid")
	}
	if resolution.EndpointDigest != digestString(identity) {
		return "", nil, fmt.Errorf("pinned public HTTPS endpoint digest does not match its identity")
	}
	if len(resolution.Addresses) == 0 {
		return "", nil, fmt.Errorf("pinned public HTTPS resolution has no addresses")
	}
	unique := make(map[netip.Addr]struct{}, len(resolution.Addresses))
	for _, address := range resolution.Addresses {
		address = address.Unmap()
		if !IsPublicAddress(address) {
			return "", nil, fmt.Errorf("pinned public HTTPS resolution contains a restricted address")
		}
		unique[address] = struct{}{}
	}
	addresses := make([]netip.Addr, 0, len(unique))
	for address := range unique {
		addresses = append(addresses, address)
	}
	sort.Slice(addresses, func(i, j int) bool { return addresses[i].Compare(addresses[j]) < 0 })
	if resolution.ResolvedAddressDigest != addressDigest(addresses) {
		return "", nil, fmt.Errorf("pinned public HTTPS address digest does not match its addresses")
	}
	return host, addresses, nil
}

func canonicalDialHost(host string) (string, error) {
	host = strings.Trim(host, "[]")
	if literal, err := netip.ParseAddr(host); err == nil {
		host = literal.Unmap().String()
	} else {
		host = strings.TrimSuffix(strings.ToLower(host), ".")
		ascii, err := idna.Lookup.ToASCII(host)
		if err != nil {
			return "", fmt.Errorf("public HTTPS dial host is invalid")
		}
		host = strings.ToLower(ascii)
	}
	if err := validatePublicHostName(host); err != nil {
		return "", err
	}
	return host, nil
}

func (p PublicHTTPSPolicy) resolveHost(ctx context.Context, host string) ([]netip.Addr, error) {
	if literal, err := netip.ParseAddr(strings.Trim(host, "[]")); err == nil {
		literal = literal.Unmap()
		if !IsPublicAddress(literal) {
			return nil, fmt.Errorf("public HTTPS endpoint resolved to a restricted address")
		}
		return []netip.Addr{literal}, nil
	}

	resolver := p.Resolver
	if resolver == nil {
		resolver = net.DefaultResolver
	}
	addresses, err := resolver.LookupNetIP(ctx, "ip", host)
	if err != nil {
		return nil, fmt.Errorf("resolve public HTTPS endpoint: %w", err)
	}
	if len(addresses) == 0 {
		return nil, fmt.Errorf("public HTTPS endpoint resolved to no addresses")
	}

	unique := make(map[netip.Addr]struct{}, len(addresses))
	for _, address := range addresses {
		address = address.Unmap()
		if !IsPublicAddress(address) {
			return nil, fmt.Errorf("public HTTPS endpoint resolved to a restricted address")
		}
		unique[address] = struct{}{}
	}
	result := make([]netip.Addr, 0, len(unique))
	for address := range unique {
		result = append(result, address)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Compare(result[j]) < 0 })
	return result, nil
}

//nolint:gocyclo // Canonicalization keeps each URL and network rejection explicit.
func canonicalPublicHTTPSEndpoint(raw string) (identity, host string, err error) {
	raw = strings.TrimSpace(raw)
	if raw == "" || len(raw) > maxEndpointLength {
		return "", "", fmt.Errorf("public HTTPS endpoint must be a bounded absolute URL")
	}
	parsed, parseErr := url.Parse(raw)
	if parseErr != nil || !parsed.IsAbs() || parsed.Host == "" || parsed.Opaque != "" {
		return "", "", fmt.Errorf("public HTTPS endpoint must be an absolute URL")
	}
	if parsed.Scheme != "https" {
		return "", "", fmt.Errorf("public endpoint must use https")
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" {
		return "", "", fmt.Errorf("public HTTPS endpoint must not contain credentials, query, or fragment components")
	}
	if parsed.RawPath != "" || strings.Contains(parsed.Path, `\`) || strings.ContainsRune(parsed.Path, '\x00') {
		return "", "", fmt.Errorf("public HTTPS endpoint path is not canonical")
	}

	host = strings.TrimSpace(parsed.Hostname())
	if host == "" {
		return "", "", fmt.Errorf("public HTTPS endpoint host is required")
	}
	if literal, literalErr := netip.ParseAddr(strings.Trim(host, "[]")); literalErr == nil {
		host = literal.Unmap().String()
	} else {
		host = strings.TrimSuffix(strings.ToLower(host), ".")
		host, err = idna.Lookup.ToASCII(host)
		if err != nil {
			return "", "", fmt.Errorf("public HTTPS endpoint host is invalid")
		}
		host = strings.ToLower(host)
	}
	if err := validatePublicHostName(host); err != nil {
		return "", "", err
	}

	port := parsed.Port()
	if port != "" {
		portNumber, portErr := strconv.Atoi(port)
		if portErr != nil || portNumber < 1 || portNumber > 65535 {
			return "", "", fmt.Errorf("public HTTPS endpoint port is invalid")
		}
	}
	hostPort := host
	if strings.Contains(host, ":") {
		hostPort = "[" + host + "]"
	}
	if port != "" && port != defaultHTTPSPort {
		hostPort = net.JoinHostPort(host, port)
	}

	canonicalPath := parsed.EscapedPath()
	if canonicalPath == "/" {
		canonicalPath = ""
	} else if canonicalPath != "" {
		if path.Clean(canonicalPath) != canonicalPath || strings.Contains(canonicalPath, "//") || strings.HasSuffix(canonicalPath, "/") {
			return "", "", fmt.Errorf("public HTTPS endpoint path is not canonical")
		}
	}
	return "https://" + hostPort + canonicalPath, host, nil
}

func validatePublicHostName(host string) error {
	host = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(strings.Trim(host, "[]"))), ".")
	if host == "" {
		return fmt.Errorf("public HTTPS endpoint host is required")
	}
	if host == "localhost" || strings.HasSuffix(host, ".localhost") || host == "local" || strings.HasSuffix(host, ".local") ||
		host == "svc" || strings.HasSuffix(host, ".svc") || strings.Contains(host, ".svc.") ||
		host == "metadata" || host == "metadata.google.internal" || host == "instance-data" || strings.HasSuffix(host, ".internal") {
		return fmt.Errorf("public HTTPS endpoint must not target local, metadata, or Kubernetes Service names")
	}
	if address, err := netip.ParseAddr(host); err == nil && !IsPublicAddress(address) {
		return fmt.Errorf("public HTTPS endpoint must not target a restricted address")
	}
	return nil
}

var restrictedAddressPrefixes = []netip.Prefix{
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

// IsPublicAddress reports whether an address is safe for a public-only endpoint.
func IsPublicAddress(address netip.Addr) bool {
	if !address.IsValid() {
		return false
	}
	address = address.Unmap()
	if !address.IsGlobalUnicast() || address.IsPrivate() || address.IsLoopback() ||
		address.IsLinkLocalUnicast() || address.IsLinkLocalMulticast() || address.IsMulticast() || address.IsUnspecified() {
		return false
	}
	for _, prefix := range restrictedAddressPrefixes {
		if prefix.Contains(address) {
			return false
		}
	}
	return true
}

// CertificateDigest returns a non-secret revision identity for the verified leaf certificate.
func CertificateDigest(state *tls.ConnectionState) (string, error) {
	if state == nil || !state.HandshakeComplete || len(state.PeerCertificates) == 0 {
		return "", fmt.Errorf("public HTTPS response did not include a verified TLS peer certificate")
	}
	return digestBytes(state.PeerCertificates[0].Raw), nil
}

func digestString(value string) string {
	return digestBytes([]byte(value))
}

func digestBytes(value []byte) string {
	sum := sha256.Sum256(value)
	return fmt.Sprintf("sha256:%x", sum[:])
}

func addressDigest(addresses []netip.Addr) string {
	values := make([]string, 0, len(addresses))
	for _, address := range addresses {
		values = append(values, address.Unmap().String())
	}
	sort.Strings(values)
	return digestString(strings.Join(values, "\n"))
}

func minPositiveDuration(current, limit time.Duration) time.Duration {
	if current <= 0 || current > limit {
		return limit
	}
	return current
}

func minDuration(current, limit time.Duration) time.Duration {
	if current <= 0 {
		return limit
	}
	if current < limit {
		return current
	}
	return limit
}
