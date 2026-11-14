/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package taskwebhook

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"slices"
	"sync/atomic"
	"testing"
	"time"
)

type resolverFunc func(ctx context.Context, host string) ([]net.IPAddr, error)

func (f resolverFunc) LookupIPAddr(ctx context.Context, host string) ([]net.IPAddr, error) {
	return f(ctx, host)
}

func staticResolver(addresses ...string) resolverFunc {
	return func(context.Context, string) ([]net.IPAddr, error) {
		result := make([]net.IPAddr, 0, len(addresses))
		for _, address := range addresses {
			result = append(result, net.IPAddr{IP: net.ParseIP(address)})
		}
		return result, nil
	}
}

// loopbackOnly stands in for "public" so tests can reach httptest servers
// while every other address stays forbidden.
func loopbackOnly(address netip.Addr) bool { return address == netip.MustParseAddr("127.0.0.1") }

func serverPort(t *testing.T, server *httptest.Server) string {
	t.Helper()
	parsed, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	return parsed.Port()
}

func TestPublicAddressPolicyRejectsNonPublicRanges(t *testing.T) {
	t.Parallel()
	for _, raw := range []string{
		"0.0.0.0", "0.1.2.3", "127.0.0.1", "10.1.2.3", "172.16.0.1", "192.168.1.1",
		"100.64.0.1", "100.127.255.254", "169.254.169.254", "224.0.0.1", "255.255.255.255",
		"192.0.2.1", "198.18.0.1", "::", "::1", "fd00::1", "fc00::1", "fe80::1", "ff02::1",
		"::ffff:10.0.0.1", "::ffff:127.0.0.1", "::ffff:169.254.169.254", "::ffff:100.64.0.1",
		"64:ff9b::a00:1", "2001:db8::1",
	} {
		if PublicAddressPolicy(netip.MustParseAddr(raw)) {
			t.Errorf("PublicAddressPolicy(%s) = true, want false", raw)
		}
	}
	for _, raw := range []string{"8.8.8.8", "1.1.1.1", "2606:4700:4700::1111", "::ffff:8.8.8.8"} {
		if !PublicAddressPolicy(netip.MustParseAddr(raw)) {
			t.Errorf("PublicAddressPolicy(%s) = false, want true", raw)
		}
	}
	if PublicAddressPolicy(netip.Addr{}) {
		t.Error("PublicAddressPolicy(invalid) = true, want false")
	}
}

func TestDialerFailsClosedOnResolutionError(t *testing.T) {
	t.Parallel()
	dialer := &Dialer{Resolver: resolverFunc(func(context.Context, string) ([]net.IPAddr, error) {
		return nil, &net.DNSError{Err: "no such host", Name: "hook.example.com", IsNotFound: true}
	})}
	if _, err := dialer.DialContext(context.Background(), "tcp", "hook.example.com:443"); !errors.Is(err, ErrResolution) {
		t.Fatalf("DialContext() error = %v, want ErrResolution", err)
	}
	empty := &Dialer{Resolver: staticResolver()}
	if _, err := empty.DialContext(context.Background(), "tcp", "hook.example.com:443"); !errors.Is(err, ErrResolution) {
		t.Fatalf("DialContext() with empty answer error = %v, want ErrResolution", err)
	}
}

func TestDialerTimeoutBoundsResolution(t *testing.T) {
	t.Parallel()
	dialer := &Dialer{
		Timeout: 50 * time.Millisecond,
		Resolver: resolverFunc(func(ctx context.Context, _ string) ([]net.IPAddr, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		}),
	}
	// net/http detaches dials from request cancellation, so the dial must
	// bound a stalled lookup itself rather than rely on the caller's context.
	done := make(chan error, 1)
	go func() {
		_, err := dialer.DialContext(context.Background(), "tcp", "hook.example.com:443")
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("DialContext() error = %v, want context.DeadlineExceeded", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("DialContext() did not bound a stalled resolution")
	}
}

func TestDialerRejectsEveryNonPublicResolvedAddress(t *testing.T) {
	t.Parallel()
	for _, raw := range []string{
		"0.0.0.0", "127.0.0.1", "10.0.0.1", "100.64.0.1", "169.254.169.254", "fd12:3456::1",
		"fe80::1", "::ffff:10.0.0.1", "::ffff:127.0.0.1", "224.0.0.251", "255.255.255.255", "::",
	} {
		t.Run(raw, func(t *testing.T) {
			t.Parallel()
			dialer := &Dialer{Resolver: staticResolver(raw)}
			if _, err := dialer.DialContext(context.Background(), "tcp", "hook.example.com:443"); !errors.Is(err, ErrAddressNotAllowed) {
				t.Fatalf("DialContext() error = %v, want ErrAddressNotAllowed", err)
			}
		})
	}
}

func TestDialerRejectsMixedAnswerSets(t *testing.T) {
	t.Parallel()
	dialer := &Dialer{Resolver: staticResolver("8.8.8.8", "10.0.0.1")}
	if _, err := dialer.DialContext(context.Background(), "tcp", "hook.example.com:443"); !errors.Is(err, ErrAddressNotAllowed) {
		t.Fatalf("DialContext() error = %v, want ErrAddressNotAllowed", err)
	}
}

func TestDialerRejectsLiteralNonPublicAddresses(t *testing.T) {
	t.Parallel()
	for _, address := range []string{"0.0.0.0:80", "127.0.0.1:80", "[::1]:80", "[fd00::1]:80", "[::ffff:10.0.0.1]:80", "100.64.0.1:80"} {
		dialer := &Dialer{Resolver: resolverFunc(func(context.Context, string) ([]net.IPAddr, error) {
			t.Fatalf("literal %s must not be resolved", address)
			return nil, nil
		})}
		if _, err := dialer.DialContext(context.Background(), "tcp", address); !errors.Is(err, ErrAddressNotAllowed) {
			t.Fatalf("DialContext(%s) error = %v, want ErrAddressNotAllowed", address, err)
		}
	}
}

func TestDialerRejectsNonTCPNetworks(t *testing.T) {
	t.Parallel()
	dialer := &Dialer{Resolver: staticResolver("8.8.8.8")}
	if _, err := dialer.DialContext(context.Background(), "udp", "hook.example.com:53"); !errors.Is(err, ErrAddressNotAllowed) {
		t.Fatalf("DialContext(udp) error = %v, want ErrAddressNotAllowed", err)
	}
}

// TestHTTPClientRechecksEveryConnectionAgainstRebinding resolves the same
// name to an allowed address and then to a forbidden one. The check runs per
// connection, so the rebound answer is refused rather than reused.
func TestHTTPClientRechecksEveryConnectionAgainstRebinding(t *testing.T) {
	t.Parallel()
	var hits atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(server.Close)

	var lookups atomic.Int32
	resolver := resolverFunc(func(context.Context, string) ([]net.IPAddr, error) {
		if lookups.Add(1) == 1 {
			return []net.IPAddr{{IP: net.ParseIP("127.0.0.1")}}, nil
		}
		return []net.IPAddr{{IP: net.ParseIP("10.0.0.7")}}, nil
	})
	client := NewHTTPClient(&Dialer{Resolver: resolver, Policy: loopbackOnly}, 0)
	target := "http://hook.example.test:" + serverPort(t, server) + "/"

	first, err := client.Post(target, "application/json", nil)
	if err != nil {
		t.Fatalf("first POST error = %v", err)
	}
	_ = first.Body.Close()
	if _, err := client.Post(target, "application/json", nil); !errors.Is(err, ErrAddressNotAllowed) {
		t.Fatalf("rebound POST error = %v, want ErrAddressNotAllowed", err)
	}
	if got := hits.Load(); got != 1 {
		t.Fatalf("server hits = %d, want 1", got)
	}
	if got := lookups.Load(); got != 2 {
		t.Fatalf("resolver lookups = %d, want one per connection (2)", got)
	}
}

func TestHTTPClientNeverFollowsRedirects(t *testing.T) {
	t.Parallel()
	var targetHits atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		targetHits.Add(1)
	}))
	t.Cleanup(target.Close)
	for _, location := range []string{target.URL, "http://169.254.169.254/latest/meta-data", "http://10.0.0.1/"} {
		redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, location, http.StatusTemporaryRedirect)
		}))
		client := NewHTTPClient(&Dialer{Policy: loopbackOnly}, 0)
		_, err := client.Post(redirector.URL, "application/json", nil)
		redirector.Close()
		if !errors.Is(err, ErrRedirectRefused) {
			t.Fatalf("redirect to %s error = %v, want ErrRedirectRefused", location, err)
		}
	}
	if got := targetHits.Load(); got != 0 {
		t.Fatalf("redirect target hits = %d, want 0", got)
	}
}

// TestHTTPClientIgnoresProxyEnvironment proves a proxy cannot dial on the
// client's behalf and bypass the address policy.
func TestHTTPClientIgnoresProxyEnvironment(t *testing.T) {
	var proxyHits atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		proxyHits.Add(1)
	}))
	t.Cleanup(proxy.Close)
	t.Setenv("HTTP_PROXY", proxy.URL)
	t.Setenv("HTTPS_PROXY", proxy.URL)
	t.Setenv("NO_PROXY", "")

	client := NewHTTPClient(&Dialer{Resolver: staticResolver("10.0.0.1")}, 0)
	if _, err := client.Post("http://hook.example.com/", "application/json", nil); !errors.Is(err, ErrAddressNotAllowed) {
		t.Fatalf("POST error = %v, want ErrAddressNotAllowed", err)
	}
	if got := proxyHits.Load(); got != 0 {
		t.Fatalf("proxy hits = %d, want 0", got)
	}
}

func TestPinnedAddressesBypassDNSAndAllowOnlyPinnedTargets(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusAccepted)
	}))
	t.Cleanup(server.Close)
	dialer := &Dialer{Resolver: resolverFunc(func(context.Context, string) ([]net.IPAddr, error) {
		t.Fatal("pinned dial must not resolve")
		return nil, nil
	})}
	client := NewHTTPClient(dialer, 0)
	target := "http://receiver.default.svc:" + serverPort(t, server) + "/"

	ctx := WithPinnedAddresses(context.Background(), []netip.Addr{netip.MustParseAddr("127.0.0.1")})
	request, _ := http.NewRequestWithContext(ctx, http.MethodPost, target, nil)
	response, err := client.Do(request)
	if err != nil {
		t.Fatalf("pinned POST error = %v", err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusAccepted {
		t.Fatalf("pinned POST status = %d", response.StatusCode)
	}

	empty := WithPinnedAddresses(context.Background(), nil)
	request, _ = http.NewRequestWithContext(empty, http.MethodPost, target, nil)
	if _, err := client.Do(request); !errors.Is(err, ErrAddressNotAllowed) {
		t.Fatalf("empty pin POST error = %v, want ErrAddressNotAllowed", err)
	}
}

// Unreachable candidates listed before a reachable one must not each consume
// a full connect timeout: one deadline covers the whole connect.
func TestDialerSharesOneDeadlineAcrossCandidates(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	t.Cleanup(server.Close)
	// TEST-NET addresses are never routed, so connects to them stall or fail.
	unreachable := []string{"192.0.2.1", "198.51.100.1", "203.0.113.1"}
	allowed := func(address netip.Addr) bool {
		return address == netip.MustParseAddr("127.0.0.1") || slices.Contains(unreachable, address.String())
	}
	const timeout = 600 * time.Millisecond
	dialer := &Dialer{
		Resolver: staticResolver(append(slices.Clone(unreachable), "127.0.0.1")...),
		Policy:   allowed,
		Timeout:  timeout,
	}
	started := time.Now()
	connection, err := dialer.DialContext(context.Background(), "tcp", net.JoinHostPort("receiver.example", serverPort(t, server)))
	if err != nil {
		t.Fatalf("reachable candidate was not dialed: %v", err)
	}
	_ = connection.Close()
	if elapsed := time.Since(started); elapsed > timeout+300*time.Millisecond {
		t.Fatalf("connect took %v; unreachable candidates exceeded the shared %v deadline", elapsed, timeout)
	}
}
