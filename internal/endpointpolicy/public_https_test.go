package endpointpolicy

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"
)

type staticResolver map[string][]netip.Addr

func (r staticResolver) LookupNetIP(_ context.Context, _ string, host string) ([]netip.Addr, error) {
	return append([]netip.Addr(nil), r[host]...), nil
}

type alternatingResolver struct {
	answers [][]netip.Addr
	calls   int
}

func (r *alternatingResolver) LookupNetIP(_ context.Context, _ string, _ string) ([]netip.Addr, error) {
	index := r.calls
	r.calls++
	if index >= len(r.answers) {
		index = len(r.answers) - 1
	}
	return append([]netip.Addr(nil), r.answers[index]...), nil
}

type recordingDialer struct {
	addresses []string
}

func (d *recordingDialer) DialContext(_ context.Context, _, address string) (net.Conn, error) {
	d.addresses = append(d.addresses, address)
	left, right := net.Pipe()
	_ = right.Close()
	return left, nil
}

type boundedAttemptDialer struct {
	mu        sync.Mutex
	addresses []string
}

func (d *boundedAttemptDialer) DialContext(ctx context.Context, _, address string) (net.Conn, error) {
	d.mu.Lock()
	d.addresses = append(d.addresses, address)
	d.mu.Unlock()
	if strings.HasPrefix(address, "1.1.1.1:") {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	left, right := net.Pipe()
	_ = right.Close()
	return left, nil
}

func (d *boundedAttemptDialer) attempted() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.addresses...)
}

func TestPublicHTTPSPolicyResolveCanonicalizesAndDigests(t *testing.T) {
	policy := PublicHTTPSPolicy{Resolver: staticResolver{
		"memory.example.com": {
			netip.MustParseAddr("2606:4700:4700::1111"),
			netip.MustParseAddr("8.8.8.8"),
			netip.MustParseAddr("8.8.8.8"),
		},
	}}
	resolved, err := policy.Resolve(context.Background(), "  https://MEMORY.example.com:443/oms  ")
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Identity != "https://memory.example.com/oms" || resolved.BaseURL != resolved.Identity {
		t.Fatalf("resolved identity = %q", resolved.Identity)
	}
	if !strings.HasPrefix(resolved.EndpointDigest, "sha256:") || !strings.HasPrefix(resolved.ResolvedAddressDigest, "sha256:") {
		t.Fatalf("resolved digests = %+v", resolved)
	}
	if len(resolved.Addresses) != 2 || resolved.Addresses[0].String() != "8.8.8.8" {
		t.Fatalf("resolved addresses = %v", resolved.Addresses)
	}
}

func TestPublicHTTPSPolicyRejectsUnsafeEndpoints(t *testing.T) {
	policy := PublicHTTPSPolicy{Resolver: staticResolver{
		"mixed.example.com": {netip.MustParseAddr("8.8.8.8"), netip.MustParseAddr("10.0.0.1")},
	}}
	for _, endpoint := range []string{
		"http://memory.example.com",
		"https://user:pass@memory.example.com",
		"https://memory.example.com?query=secret",
		"https://memory.example.com#fragment",
		"https://127.0.0.1",
		"https://169.254.169.254",
		"https://adapter.default.svc",
		"https://metadata.google.internal",
		"https://memory.localhost",
		"https://memory.local",
		"https://mixed.example.com",
		"https://memory.example.com/a/../b",
	} {
		if _, err := policy.Resolve(context.Background(), endpoint); err == nil {
			t.Errorf("Resolve(%q) unexpectedly succeeded", endpoint)
		}
	}
}

func TestPublicHTTPSPolicyDialValidatesEveryAnswerBeforeDial(t *testing.T) {
	dialer := &recordingDialer{}
	policy := PublicHTTPSPolicy{
		Resolver: staticResolver{
			"mixed.example.com": {netip.MustParseAddr("8.8.8.8"), netip.MustParseAddr("192.168.1.1")},
			"safe.example.com":  {netip.MustParseAddr("8.8.4.4")},
		},
		Dialer: dialer,
	}
	if _, err := policy.DialContext(context.Background(), "tcp", "mixed.example.com:443"); err == nil {
		t.Fatal("mixed public/private DNS answers unexpectedly dialed")
	}
	if len(dialer.addresses) != 0 {
		t.Fatalf("restricted resolution reached dialer: %v", dialer.addresses)
	}
	connection, err := policy.DialContext(context.Background(), "tcp", "safe.example.com:443")
	if err != nil {
		t.Fatal(err)
	}
	_ = connection.Close()
	if len(dialer.addresses) != 1 || dialer.addresses[0] != "8.8.4.4:443" {
		t.Fatalf("dialed addresses = %v", dialer.addresses)
	}
}

func TestPublicHTTPSPolicyHTTPClientDisablesAmbientRoutingAndRedirects(t *testing.T) {
	policy := PublicHTTPSPolicy{}
	client, err := policy.NewHTTPClient(nil, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	transport, ok := client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport = %T", client.Transport)
	}
	if transport.Proxy != nil || transport.DialContext == nil {
		t.Fatalf("transport does not enforce direct validated dialing: %#v", transport)
	}
	if !transport.DisableCompression {
		t.Fatal("transparent response decompression is enabled")
	}
	if !transport.DisableKeepAlives || transport.MaxIdleConns != 0 || transport.MaxIdleConnsPerHost >= 0 {
		t.Fatalf("short-lived client can retain idle connections: %#v", transport)
	}
	if transport.TLSClientConfig == nil || transport.TLSClientConfig.MinVersion < tls.VersionTLS12 || transport.TLSClientConfig.InsecureSkipVerify {
		t.Fatalf("TLS config = %#v", transport.TLSClientConfig)
	}
	if client.Jar != nil {
		t.Fatal("public control-plane client retained a cookie jar")
	}
	if err := client.CheckRedirect(&http.Request{}, nil); err != http.ErrUseLastResponse {
		t.Fatalf("redirect policy error = %v", err)
	}
}

func TestPublicHTTPSPolicyDialBudgetsEveryValidatedAddress(t *testing.T) {
	dialer := &boundedAttemptDialer{}
	policy := PublicHTTPSPolicy{Dialer: dialer}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	connection, err := policy.dialAddresses(ctx, "tcp", "443", []netip.Addr{
		netip.MustParseAddr("1.1.1.1"),
		netip.MustParseAddr("8.8.8.8"),
	})
	if err != nil {
		t.Fatalf("dialAddresses() error = %v", err)
	}
	_ = connection.Close()
	if got := dialer.attempted(); len(got) != 2 || got[0] != "1.1.1.1:443" || got[1] != "8.8.8.8:443" {
		t.Fatalf("attempted addresses = %v", got)
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		t.Fatal("first address consumed the complete overall dial budget")
	}
}

func TestPublicHTTPSPolicyPinnedHTTPClientUsesValidatedAddressesWithoutResolvingAgain(t *testing.T) {
	resolver := &alternatingResolver{answers: [][]netip.Addr{
		{netip.MustParseAddr("8.8.8.8")},
		{netip.MustParseAddr("1.1.1.1")},
	}}
	dialer := &recordingDialer{}
	policy := PublicHTTPSPolicy{Resolver: resolver, Dialer: dialer}
	resolution, err := policy.Resolve(context.Background(), "https://memory.example.com")
	if err != nil {
		t.Fatal(err)
	}
	client, err := policy.NewPinnedHTTPClient(nil, time.Second, resolution)
	if err != nil {
		t.Fatal(err)
	}
	transport := client.Transport.(*http.Transport)
	connection, err := transport.DialContext(context.Background(), "tcp", "memory.example.com:443")
	if err != nil {
		t.Fatal(err)
	}
	_ = connection.Close()
	if resolver.calls != 1 {
		t.Fatalf("resolver calls = %d, want exactly the identity-resolution call", resolver.calls)
	}
	if len(dialer.addresses) != 1 || dialer.addresses[0] != "8.8.8.8:443" {
		t.Fatalf("pinned dial addresses = %v", dialer.addresses)
	}
	if transport.TLSClientConfig.ServerName != "" {
		t.Fatalf("TLS server name override = %q; request hostname must drive SNI verification", transport.TLSClientConfig.ServerName)
	}
	if _, err := transport.DialContext(context.Background(), "tcp", "other.example.com:443"); err == nil {
		t.Fatal("pinned client dialed a host outside the validated endpoint identity")
	}
}

func TestPublicHTTPSPolicyPinnedHTTPClientRejectsTamperedResolution(t *testing.T) {
	policy := PublicHTTPSPolicy{Resolver: staticResolver{
		"memory.example.com": {netip.MustParseAddr("8.8.8.8")},
	}}
	resolution, err := policy.Resolve(context.Background(), "https://memory.example.com")
	if err != nil {
		t.Fatal(err)
	}
	resolution.Addresses = []netip.Addr{netip.MustParseAddr("10.0.0.1")}
	if _, err := policy.NewPinnedHTTPClient(nil, time.Second, resolution); err == nil {
		t.Fatal("pinned client accepted a restricted address under the validated digest")
	}
}

func TestPublicHTTPSPolicyRejectsUnsafeCustomTransport(t *testing.T) {
	policy := PublicHTTPSPolicy{}
	base := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}} //nolint:gosec
	if _, err := policy.NewHTTPClient(base, time.Second); err == nil {
		t.Fatal("client with InsecureSkipVerify unexpectedly accepted")
	}
}

func TestIsPublicAddress(t *testing.T) {
	for raw, want := range map[string]bool{
		"8.8.8.8": true, "2606:4700:4700::1111": true,
		"127.0.0.1": false, "10.0.0.1": false, "100.64.0.1": false,
		"169.254.169.254": false, "::1": false, "fc00::1": false,
		"64:ff9b:1::1": false, "64:ff9b::a00:1": false, "fec0::1": false,
		"2002:a00:1::1": false,
	} {
		if got := IsPublicAddress(netip.MustParseAddr(raw)); got != want {
			t.Errorf("IsPublicAddress(%s) = %v, want %v", raw, got, want)
		}
	}
}

func TestCertificateDigestRequiresCompletedTLS(t *testing.T) {
	if _, err := CertificateDigest(nil); err == nil {
		t.Fatal("nil TLS state unexpectedly accepted")
	}
	state := &tls.ConnectionState{HandshakeComplete: true, PeerCertificates: []*x509.Certificate{{Raw: []byte("certificate")}}}
	digest, err := CertificateDigest(state)
	if err != nil || !strings.HasPrefix(digest, "sha256:") {
		t.Fatalf("CertificateDigest() = %q, %v", digest, err)
	}
}
