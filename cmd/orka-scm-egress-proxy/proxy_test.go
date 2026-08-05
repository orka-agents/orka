package main

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const testProxyToken = "0123456789abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ-_"

func TestSCMEgressProxyRejectsUnlistedProviderHost(t *testing.T) {
	var lookups atomic.Int32
	proxy := newTestSCMProxy(t, proxyConfig{
		AllowedHosts: map[string]struct{}{"github.com": {}},
		Resolver: resolverFunc(func(context.Context, string, string) ([]netip.Addr, error) {
			lookups.Add(1)
			return []netip.Addr{netip.MustParseAddr("1.1.1.1")}, nil
		}),
	})
	request := connectRequest("api.openai.com:443")
	response := httptest.NewRecorder()
	proxy.ServeHTTP(response, request)
	if response.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusForbidden)
	}
	if lookups.Load() != 0 {
		t.Fatalf("DNS lookups = %d, want 0", lookups.Load())
	}
}

func TestSCMEgressProxyRejectsPrivateAndRebindingAnswers(t *testing.T) {
	for name, addresses := range map[string][]netip.Addr{
		"private": {
			netip.MustParseAddr("10.0.0.8"),
		},
		"loopback": {
			netip.MustParseAddr("127.0.0.1"),
		},
		"metadata": {
			netip.MustParseAddr("169.254.169.254"),
		},
		"mixed-public-private": {
			netip.MustParseAddr("1.1.1.1"),
			netip.MustParseAddr("192.168.1.7"),
		},
	} {
		t.Run(name, func(t *testing.T) {
			var dials atomic.Int32
			proxy := newTestSCMProxy(t, proxyConfig{
				AllowedHosts: map[string]struct{}{"github.com": {}},
				Resolver: resolverFunc(func(context.Context, string, string) ([]netip.Addr, error) {
					return addresses, nil
				}),
				Dialer: dialerFunc(func(context.Context, string, string) (net.Conn, error) {
					dials.Add(1)
					return nil, fmt.Errorf("unexpected dial")
				}),
			})
			response := httptest.NewRecorder()
			proxy.ServeHTTP(response, connectRequest("github.com:443"))
			if response.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want %d", response.Code, http.StatusForbidden)
			}
			if dials.Load() != 0 {
				t.Fatalf("dials = %d, want 0", dials.Load())
			}
		})
	}
}

func TestSCMEgressProxyAllowsAuthenticatedGitHubCONNECT(t *testing.T) {
	upstream, upstreamAddress := startEchoServer(t)
	defer func() { _ = upstream.Close() }()
	publicAddress := netip.MustParseAddr("1.1.1.1")
	proxy := newTestSCMProxy(t, proxyConfig{
		AllowedHosts: map[string]struct{}{"github.com": {}},
		Resolver: resolverFunc(func(_ context.Context, network, host string) ([]netip.Addr, error) {
			if network != "ip" || host != "github.com" {
				t.Fatalf("lookup = %s %s", network, host)
			}
			return []netip.Addr{publicAddress}, nil
		}),
		Dialer: localTestDialer(t, upstreamAddress, publicAddress),
	})
	server := httptest.NewServer(proxy)
	defer server.Close()

	connection, err := net.DialTimeout("tcp", strings.TrimPrefix(server.URL, "http://"), time.Second)
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	defer func() { _ = connection.Close() }()
	if _, err := fmt.Fprintf(
		connection,
		"CONNECT github.com:443 HTTP/1.1\r\nHost: github.com:443\r\nProxy-Authorization: %s\r\n\r\n",
		proxyAuthorization(),
	); err != nil {
		t.Fatalf("write CONNECT: %v", err)
	}
	reader := bufio.NewReader(connection)
	status, err := reader.ReadString('\n')
	if err != nil {
		t.Fatalf("read CONNECT status: %v", err)
	}
	if status != "HTTP/1.1 200 Connection Established\r\n" {
		t.Fatalf("CONNECT status = %q", status)
	}
	for {
		line, readErr := reader.ReadString('\n')
		if readErr != nil {
			t.Fatalf("read CONNECT header: %v", readErr)
		}
		if line == "\r\n" {
			break
		}
	}
	if _, err := connection.Write([]byte("github-connect-ok")); err != nil {
		t.Fatalf("write tunnel payload: %v", err)
	}
	payload := make([]byte, len("github-connect-ok"))
	if _, err := io.ReadFull(reader, payload); err != nil {
		t.Fatalf("read tunnel payload: %v", err)
	}
	if string(payload) != "github-connect-ok" {
		t.Fatalf("tunnel payload = %q", payload)
	}
}

func TestSCMEgressProxyRejectsRedirect(t *testing.T) {
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Location", "https://github.com/redirected")
		writer.WriteHeader(http.StatusFound)
	}))
	defer upstream.Close()
	proxy := testForwardProxy(t, upstream, 1024, 1024)
	request := httptest.NewRequest(http.MethodGet, "https://github.com/start", nil)
	request.Header.Set("Proxy-Authorization", proxyAuthorization())
	response := httptest.NewRecorder()
	proxy.ServeHTTP(response, request)
	if response.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusBadGateway)
	}
}

func TestSCMEgressProxyRejectsOversizedHeaders(t *testing.T) {
	proxy := newTestSCMProxy(t, proxyConfig{
		AllowedHosts:          map[string]struct{}{"github.com": {}},
		MaxRequestHeaderBytes: 1024,
	})
	request := connectRequest("github.com:443")
	request.Header.Set("X-Oversized", strings.Repeat("x", 2048))
	response := httptest.NewRecorder()
	proxy.ServeHTTP(response, request)
	if response.Code != http.StatusRequestHeaderFieldsTooLarge {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusRequestHeaderFieldsTooLarge)
	}
}

func TestSCMEgressProxyRejectsOversizedRequestsAndResponses(t *testing.T) {
	t.Run("known request", func(t *testing.T) {
		var lookups atomic.Int32
		proxy := newTestSCMProxy(t, proxyConfig{
			AllowedHosts:    map[string]struct{}{"github.com": {}},
			MaxRequestBytes: 4,
			Resolver: resolverFunc(func(context.Context, string, string) ([]netip.Addr, error) {
				lookups.Add(1)
				return []netip.Addr{netip.MustParseAddr("1.1.1.1")}, nil
			}),
		})
		request := httptest.NewRequest(http.MethodPost, "https://github.com/upload", strings.NewReader("12345"))
		request.Header.Set("Proxy-Authorization", proxyAuthorization())
		response := httptest.NewRecorder()
		proxy.ServeHTTP(response, request)
		if response.Code != http.StatusRequestEntityTooLarge {
			t.Fatalf("status = %d, want %d", response.Code, http.StatusRequestEntityTooLarge)
		}
		if lookups.Load() != 0 {
			t.Fatalf("DNS lookups = %d, want 0", lookups.Load())
		}
	})

	t.Run("streamed request", func(t *testing.T) {
		proxy := newTestSCMProxy(t, proxyConfig{
			AllowedHosts:    map[string]struct{}{"github.com": {}},
			MaxRequestBytes: 4,
		})
		request := httptest.NewRequest(http.MethodPost, "https://github.com/upload", strings.NewReader("12345"))
		request.ContentLength = -1
		request.Header.Set("Proxy-Authorization", proxyAuthorization())
		response := httptest.NewRecorder()
		proxy.ServeHTTP(response, request)
		if response.Code != http.StatusRequestEntityTooLarge {
			t.Fatalf("status = %d, want %d", response.Code, http.StatusRequestEntityTooLarge)
		}
	})

	t.Run("response", func(t *testing.T) {
		upstream := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(writer, "12345")
		}))
		defer upstream.Close()
		proxy := testForwardProxy(t, upstream, 1024, 4)
		request := httptest.NewRequest(http.MethodGet, "https://github.com/download", nil)
		request.Header.Set("Proxy-Authorization", proxyAuthorization())
		response := httptest.NewRecorder()
		proxy.ServeHTTP(response, request)
		if response.Code != http.StatusBadGateway {
			t.Fatalf("status = %d, want %d", response.Code, http.StatusBadGateway)
		}
	})
}

func TestSCMEgressProxyRejectsPlainHTTPAndNon443Targets(t *testing.T) {
	proxy := newTestSCMProxy(t, proxyConfig{AllowedHosts: map[string]struct{}{"github.com": {}}})

	plainRequest := httptest.NewRequest(http.MethodGet, "http://github.com/repository", nil)
	plainRequest.Header.Set("Proxy-Authorization", proxyAuthorization())
	plainResponse := httptest.NewRecorder()
	proxy.ServeHTTP(plainResponse, plainRequest)
	if plainResponse.Code != http.StatusForbidden {
		t.Fatalf("plain HTTP status = %d, want %d", plainResponse.Code, http.StatusForbidden)
	}

	connectResponse := httptest.NewRecorder()
	proxy.ServeHTTP(connectResponse, connectRequest("github.com:8443"))
	if connectResponse.Code != http.StatusForbidden {
		t.Fatalf("non-443 CONNECT status = %d, want %d", connectResponse.Code, http.StatusForbidden)
	}
}

func TestSCMEgressProxyRejectsConnectedPeerMismatch(t *testing.T) {
	listener, localAddress := startEchoServer(t)
	defer func() { _ = listener.Close() }()
	resolved := netip.MustParseAddr("1.1.1.1")
	mismatched := netip.MustParseAddr("8.8.8.8")
	proxy := newTestSCMProxy(t, proxyConfig{
		AllowedHosts: map[string]struct{}{"github.com": {}},
		Resolver: resolverFunc(func(context.Context, string, string) ([]netip.Addr, error) {
			return []netip.Addr{resolved}, nil
		}),
		Dialer: dialerFunc(func(ctx context.Context, network, address string) (net.Conn, error) {
			if network != "tcp" || address != net.JoinHostPort(resolved.String(), "443") {
				t.Fatalf("dial = %s %s", network, address)
			}
			dialer := net.Dialer{}
			connection, err := dialer.DialContext(ctx, "tcp", localAddress)
			if err != nil {
				return nil, err
			}
			return &remoteAddressConn{
				Conn: connection,
				remote: &net.TCPAddr{
					IP: net.ParseIP(mismatched.String()), Port: 443,
				},
			}, nil
		}),
	})
	response := httptest.NewRecorder()
	proxy.ServeHTTP(response, connectRequest("github.com:443"))
	if response.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusForbidden)
	}
}

func TestSCMEgressProxyRequiresAuthentication(t *testing.T) {
	proxy := newTestSCMProxy(t, proxyConfig{AllowedHosts: map[string]struct{}{"github.com": {}}})
	request := connectRequest("github.com:443")
	request.Header.Del("Proxy-Authorization")
	response := httptest.NewRecorder()
	proxy.ServeHTTP(response, request)
	if response.Code != http.StatusProxyAuthRequired {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusProxyAuthRequired)
	}
}

func TestAllowedHostsRequireExactLowercaseDNSNames(t *testing.T) {
	for _, value := range []string{"GitHub.com", "github.com.", "127.0.0.1", "*.github.com", "github"} {
		t.Run(value, func(t *testing.T) {
			if _, err := allowedHosts(value, ""); err == nil {
				t.Fatal("allowedHosts accepted unsafe host")
			}
		})
	}
	hosts, err := allowedHosts("github.com", "https://api.github.com")
	if err != nil {
		t.Fatalf("allowedHosts: %v", err)
	}
	for _, host := range []string{"github.com", "api.github.com"} {
		if _, ok := hosts[host]; !ok {
			t.Fatalf("host %q was not allowed", host)
		}
	}
}

func newTestSCMProxy(t *testing.T, config proxyConfig) *scmEgressProxy {
	t.Helper()
	authenticator, err := newProxyAuthenticator([]byte(testProxyToken))
	if err != nil {
		t.Fatalf("new authenticator: %v", err)
	}
	proxy, err := newSCMEgressProxy(config, authenticator)
	if err != nil {
		t.Fatalf("new proxy: %v", err)
	}
	return proxy
}

func connectRequest(target string) *http.Request {
	request := httptest.NewRequest(http.MethodConnect, "http://proxy.invalid", nil)
	request.Host = target
	request.Header.Set("Proxy-Authorization", proxyAuthorization())
	return request
}

func proxyAuthorization() string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(proxyUsername+":"+testProxyToken))
}

type resolverFunc func(context.Context, string, string) ([]netip.Addr, error)

func (function resolverFunc) LookupNetIP(
	ctx context.Context,
	network string,
	host string,
) ([]netip.Addr, error) {
	return function(ctx, network, host)
}

type dialerFunc func(context.Context, string, string) (net.Conn, error)

func (function dialerFunc) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	return function(ctx, network, address)
}

type remoteAddressConn struct {
	net.Conn
	remote net.Addr
}

func (connection *remoteAddressConn) RemoteAddr() net.Addr { return connection.remote }

func localTestDialer(t *testing.T, localAddress string, publicAddress netip.Addr) dialerFunc {
	t.Helper()
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		if network != "tcp" || address != net.JoinHostPort(publicAddress.String(), "443") {
			t.Fatalf("dial = %s %s", network, address)
		}
		dialer := net.Dialer{}
		connection, err := dialer.DialContext(ctx, "tcp", localAddress)
		if err != nil {
			return nil, err
		}
		return &remoteAddressConn{
			Conn: connection,
			remote: &net.TCPAddr{
				IP:   net.ParseIP(publicAddress.String()),
				Port: 443,
			},
		}, nil
	}
}

func testForwardProxy(
	t *testing.T,
	upstream *httptest.Server,
	maxRequestBytes int64,
	maxResponseBytes int64,
) *scmEgressProxy {
	t.Helper()
	upstreamAddress := strings.TrimPrefix(upstream.URL, "https://")
	publicAddress := netip.MustParseAddr("1.1.1.1")
	proxy := newTestSCMProxy(t, proxyConfig{
		AllowedHosts:     map[string]struct{}{"github.com": {}},
		MaxRequestBytes:  maxRequestBytes,
		MaxResponseBytes: maxResponseBytes,
		Resolver: resolverFunc(func(context.Context, string, string) ([]netip.Addr, error) {
			return []netip.Addr{publicAddress}, nil
		}),
		Dialer: localTestDialer(t, upstreamAddress, publicAddress),
	})
	proxy.client = proxy.newForwardClient(&tls.Config{InsecureSkipVerify: true}) //nolint:gosec // test-only TLS server
	return proxy
}

func startEchoServer(t *testing.T) (net.Listener, string) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen upstream: %v", err)
	}
	go func() {
		connection, acceptErr := listener.Accept()
		if acceptErr != nil {
			return
		}
		defer func() { _ = connection.Close() }()
		_, _ = io.Copy(connection, connection)
	}()
	return listener, listener.Addr().String()
}
