//go:build linux

package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	pv "github.com/orka-agents/orka/internal/patchverification"
	"github.com/stretchr/testify/require"
)

func sourceProbeInput() pv.ApplicationBoundaryInput {
	return pv.ApplicationBoundaryInput{
		Version: pv.ApplicationProtocolVersion, ID: "source-probe", TaskUID: "probe-task",
		Target: pv.ApplicationTarget{
			ClusterUID: "target-cluster", Namespace: "source-probe", NamespaceUID: "namespace-uid",
		},
		CanaryHost: "127.0.0.1", CanaryPort: 443, PositiveControl: true, SourceHost: "github.com",
	}
}

func sourceProbeServer(
	t *testing.T, name string, status int, oversized bool,
) (*httptest.Server, *x509.CertPool, *atomic.Int32) {
	t.Helper()
	public, private, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: name}, DNSNames: []string{name},
		NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, public, private)
	require.NoError(t, err)
	certificate, err := x509.ParseCertificate(der)
	require.NoError(t, err)
	roots := x509.NewCertPool()
	roots.AddCert(certificate)
	calls := new(atomic.Int32)
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		calls.Add(1)
		if request.Method != http.MethodHead || request.URL.RequestURI() != "/" ||
			request.Host != "github.com" || request.TLS == nil || request.TLS.ServerName != "github.com" ||
			request.Header.Get("Authorization") != "" || request.Header.Get("Proxy-Authorization") != "" ||
			request.Header.Get("Cookie") != "" || request.ContentLength != 0 {
			writer.WriteHeader(http.StatusBadRequest)
			return
		}
		if status == http.StatusFound {
			writer.Header().Set("Location", "https://not-admitted.invalid/")
		}
		if oversized {
			writer.Header().Set("X-Bounded-Test", strings.Repeat("x", 16384))
		}
		writer.WriteHeader(status)
	}))
	server.Config.ErrorLog = log.New(io.Discard, "", 0)
	server.TLS = &tls.Config{
		Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: private}},
		MinVersion:   tls.VersionTLS12,
	}
	server.StartTLS()
	t.Cleanup(server.Close)
	return server, roots, calls
}

func sourceProbeDial(server *httptest.Server) func(context.Context, string, string) (net.Conn, error) {
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		if network != "tcp" || address != "127.0.0.1:443" {
			return nil, errors.New("source probe dialed outside the exact relay")
		}
		return (&net.Dialer{}).DialContext(ctx, network, server.Listener.Addr().String())
	}
}

func TestSourceHTTPSProbeVerifiesPublicHostThroughExactRelayWithoutCredentials(t *testing.T) {
	t.Setenv("HTTP_PROXY", "http://not-admitted.invalid")
	t.Setenv("HTTPS_PROXY", "http://not-admitted.invalid")
	for _, status := range []int{http.StatusOK, http.StatusFound} {
		server, roots, calls := sourceProbeServer(t, "github.com", status, false)
		actual, err := sourceHTTPSProbeWithTransport(t.Context(), sourceProbeInput(), roots, sourceProbeDial(server))
		require.NoError(t, err)
		require.Equal(t, status, actual)
		require.Equal(t, int32(1), calls.Load(), "redirects and proxy settings must not widen the public HEAD probe")
	}
}

func TestSourceHTTPSProbeFailsClosedOnWrongIdentityUnavailableAndOversizedResponse(t *testing.T) {
	for _, test := range []struct {
		name      string
		host      string
		status    int
		oversized bool
		untrusted bool
	}{
		{name: "wrong certificate name", host: "api.github.com", status: 200},
		{name: "untrusted certificate", host: "github.com", status: 200, untrusted: true},
		{name: "unavailable public service", host: "github.com", status: 503},
		{name: "oversized headers", host: "github.com", status: 200, oversized: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			server, roots, _ := sourceProbeServer(t, test.host, test.status, test.oversized)
			if test.untrusted {
				roots = x509.NewCertPool()
			}
			_, err := sourceHTTPSProbeWithTransport(t.Context(), sourceProbeInput(), roots, sourceProbeDial(server))
			require.Error(t, err)
		})
	}
	input := sourceProbeInput()
	called := false
	dial := func(context.Context, string, string) (net.Conn, error) {
		called = true
		return nil, net.ErrClosed
	}
	input.PositiveControl = false
	_, err := sourceHTTPSProbeWithTransport(t.Context(), input, x509.NewCertPool(), dial)
	require.Error(t, err)
	require.False(t, called, "negative subject probes must remain TCP-denial probes, not HTTPS requests")
	input.PositiveControl, input.SourceHost = true, "not-admitted.invalid"
	_, err = sourceHTTPSProbeWithTransport(t.Context(), input, x509.NewCertPool(), dial)
	require.Error(t, err)
	require.False(t, called)
}
