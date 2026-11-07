/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

// Package taskwebhook implements the outbound transport and signing contract
// for Task completion webhooks: a dialer that enforces the destination policy
// on every connection, and Ed25519-signed per-attempt delivery tokens whose
// public keys are published as a JWKS.
package taskwebhook

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"slices"
	"syscall"
	"time"

	"github.com/orka-agents/orka/internal/tokenexchange"
)

const (
	// maxResolvedAddresses bounds how many addresses one host may resolve to.
	maxResolvedAddresses = 16
	defaultDialTimeout   = 10 * time.Second
	// DefaultRequestTimeout bounds one delivery attempt end to end.
	DefaultRequestTimeout = 30 * time.Second
	maxResponseHeaderSize = 64 << 10
)

var (
	// ErrResolution reports that the destination host could not be resolved.
	// Resolution failure is never treated as permission to connect.
	ErrResolution = errors.New("webhook destination could not be resolved")
	// ErrAddressNotAllowed reports a destination address outside the policy.
	ErrAddressNotAllowed = errors.New("webhook destination address is not allowed")
	// ErrRedirectRefused reports that the receiver answered with a redirect.
	ErrRedirectRefused = errors.New("webhook redirects are not followed")
)

// Resolver resolves a host name to IP addresses.
type Resolver interface {
	LookupIPAddr(ctx context.Context, host string) ([]net.IPAddr, error)
}

// AddressPolicy reports whether a connection to address is permitted.
type AddressPolicy func(address netip.Addr) bool

// PublicAddressPolicy permits only globally routable unicast addresses. It
// reuses the outbound-credential policy, which rejects loopback, RFC 1918,
// CGNAT, link-local, unique-local, multicast, unspecified, broadcast,
// documentation, benchmarking, and other IANA special-use ranges, after
// unmapping IPv4-mapped IPv6 addresses.
func PublicAddressPolicy(address netip.Addr) bool {
	if !address.IsValid() {
		return false
	}
	return tokenexchange.IsPublicAddress(net.IP(address.Unmap().AsSlice()))
}

type pinnedAddressesKey struct{}

// WithPinnedAddresses returns a context whose dials go only to addresses,
// without DNS and without the public-address policy. The controller uses it
// for same-namespace Services after validating the Service object, pinning the
// connection to that Service's ClusterIPs.
func WithPinnedAddresses(ctx context.Context, addresses []netip.Addr) context.Context {
	pinned := make([]netip.Addr, 0, len(addresses))
	for _, address := range addresses {
		if address.IsValid() {
			pinned = append(pinned, address.Unmap())
		}
	}
	return context.WithValue(ctx, pinnedAddressesKey{}, pinned)
}

func pinnedAddresses(ctx context.Context) ([]netip.Addr, bool) {
	pinned, ok := ctx.Value(pinnedAddressesKey{}).([]netip.Addr)
	return pinned, ok
}

// Dialer resolves and dials webhook destinations while enforcing an address
// policy at connect time.
type Dialer struct {
	// Resolver resolves host names. Defaults to net.DefaultResolver.
	Resolver Resolver
	// Policy permits destination addresses. Defaults to PublicAddressPolicy.
	Policy AddressPolicy
	// Timeout bounds the whole connect, including name resolution and every
	// candidate address, shared equally among the candidates still untried.
	// Defaults to 10s.
	Timeout time.Duration
}

// DialContext resolves address and connects to one permitted address. Every
// resolved address must satisfy the policy, otherwise the whole set is
// rejected so a rebinding answer that mixes public and private records cannot
// be steered. The socket-level Control hook then re-checks the exact address
// being connected, so no code path can connect to an address the policy did
// not admit.
func (d *Dialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	switch network {
	case "tcp", "tcp4", "tcp6":
	default:
		return nil, fmt.Errorf("%w: network %q", ErrAddressNotAllowed, network)
	}
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, fmt.Errorf("%w: invalid address", ErrAddressNotAllowed)
	}
	timeout := d.Timeout
	if timeout <= 0 {
		timeout = defaultDialTimeout
	}
	// One deadline bounds the whole connect, including resolution: net/http
	// detaches dials from request cancellation, so without it a stalled
	// lookup would outlive the request. Unreachable candidates listed first
	// cannot spend the deadline before a reachable one is tried; each
	// candidate gets an equal share of the remaining time.
	dialCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	candidates, allowed, err := d.candidates(dialCtx, host)
	if err != nil {
		return nil, err
	}
	dialer := &net.Dialer{
		ControlContext: func(_ context.Context, _ string, address string, _ syscall.RawConn) error {
			remote, parseErr := netip.ParseAddrPort(address)
			if parseErr != nil || !allowed(remote.Addr().Unmap()) {
				return ErrAddressNotAllowed
			}
			return nil
		},
	}
	var lastErr error
	for i, candidate := range candidates {
		deadline, _ := dialCtx.Deadline()
		attemptCtx, cancelAttempt := context.WithTimeout(dialCtx, time.Until(deadline)/time.Duration(len(candidates)-i))
		connection, dialErr := dialer.DialContext(attemptCtx, network, net.JoinHostPort(candidate.String(), port))
		cancelAttempt()
		if dialErr == nil {
			return connection, nil
		}
		lastErr = dialErr
		if errors.Is(dialErr, ErrAddressNotAllowed) {
			return nil, ErrAddressNotAllowed
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if dialCtx.Err() != nil {
			break
		}
	}
	if lastErr == nil {
		lastErr = ErrResolution
	}
	return nil, lastErr
}

func (d *Dialer) candidates(ctx context.Context, host string) ([]netip.Addr, AddressPolicy, error) {
	if pinned, ok := pinnedAddresses(ctx); ok {
		if len(pinned) == 0 {
			return nil, nil, ErrAddressNotAllowed
		}
		allowed := func(address netip.Addr) bool { return slices.Contains(pinned, address.Unmap()) }
		return pinned, allowed, nil
	}
	policy := d.Policy
	if policy == nil {
		policy = PublicAddressPolicy
	}
	if literal, err := netip.ParseAddr(host); err == nil {
		literal = literal.Unmap()
		if literal.Zone() != "" || !policy(literal) {
			return nil, nil, ErrAddressNotAllowed
		}
		return []netip.Addr{literal}, policy, nil
	}
	resolver := d.Resolver
	if resolver == nil {
		resolver = net.DefaultResolver
	}
	resolved, err := resolver.LookupIPAddr(ctx, host)
	if err != nil {
		if ctx.Err() != nil {
			return nil, nil, ctx.Err()
		}
		return nil, nil, ErrResolution
	}
	if len(resolved) == 0 || len(resolved) > maxResolvedAddresses {
		return nil, nil, ErrResolution
	}
	candidates := make([]netip.Addr, 0, len(resolved))
	for _, entry := range resolved {
		address, ok := netip.AddrFromSlice(entry.IP)
		if !ok || entry.Zone != "" {
			return nil, nil, ErrAddressNotAllowed
		}
		address = address.Unmap()
		if !policy(address) {
			return nil, nil, ErrAddressNotAllowed
		}
		candidates = append(candidates, address)
	}
	return candidates, policy, nil
}

// NewHTTPClient returns a client for webhook delivery. It never uses a proxy
// (a proxy would dial on the client's behalf and bypass the address policy),
// never follows redirects, and never reuses connections, so every request
// re-resolves and re-checks its destination.
func NewHTTPClient(dialer *Dialer, timeout time.Duration) *http.Client {
	if dialer == nil {
		dialer = &Dialer{}
	}
	if timeout <= 0 {
		timeout = DefaultRequestTimeout
	}
	transport := &http.Transport{
		Proxy:                  nil,
		DialContext:            dialer.DialContext,
		DisableKeepAlives:      true,
		TLSHandshakeTimeout:    10 * time.Second,
		ResponseHeaderTimeout:  timeout,
		MaxResponseHeaderBytes: maxResponseHeaderSize,
	}
	return &http.Client{
		Transport: transport,
		Timeout:   timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return ErrRedirectRefused
		},
	}
}
