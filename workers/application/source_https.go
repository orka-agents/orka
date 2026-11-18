//go:build linux

package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net"
	"net/http"
	"os"
	"time"

	pv "github.com/orka-agents/orka/internal/patchverification"
)

func sourceHTTPSProbe(ctx context.Context, input pv.ApplicationBoundaryInput) (int, error) {
	// Use only the public CA bundle in the pinned worker image, not kubeconfig,
	// target-cluster roots or inherited SSL_CERT_FILE/SSL_CERT_DIR overrides.
	content, err := os.ReadFile("/etc/ssl/certs/ca-certificates.crt")
	roots := x509.NewCertPool()
	if err != nil || len(content) > 2<<20 || !roots.AppendCertsFromPEM(content) {
		return 0, errors.New("application source public TLS trust is unavailable")
	}
	return sourceHTTPSProbeWithTransport(ctx, input, roots, (&net.Dialer{}).DialContext)
}

func sourceHTTPSProbeWithTransport(ctx context.Context, input pv.ApplicationBoundaryInput, roots *x509.CertPool,
	dial func(context.Context, string, string) (net.Conn, error)) (int, error) {
	if _, err := pv.ApplicationBoundaryInputDigest(input); err != nil || !input.PositiveControl ||
		input.SourceHost == "" || roots == nil || dial == nil {
		return 0, pv.ErrEvidence
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	transport := &http.Transport{
		Proxy: nil, DisableCompression: true, DisableKeepAlives: true,
		MaxResponseHeaderBytes: 8192, MaxConnsPerHost: 1,
		TLSHandshakeTimeout: 3 * time.Second, ResponseHeaderTimeout: 3 * time.Second,
		TLSClientConfig: &tls.Config{RootCAs: roots, ServerName: input.SourceHost, MinVersion: tls.VersionTLS12},
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			if network != "tcp" || address != net.JoinHostPort(input.SourceHost, "443") {
				return nil, errors.New("application source probe cannot dial another authority")
			}
			return dial(ctx, "tcp", net.JoinHostPort(input.CanaryHost, "443"))
		},
	}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 5 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	request, err := http.NewRequestWithContext(ctx, http.MethodHead, "https://"+input.SourceHost+"/", nil)
	if err != nil {
		return 0, pv.ErrEvidence
	}
	request.Header.Set("User-Agent", "orka-application-source-probe")
	response, err := client.Do(request)
	if err != nil {
		return 0, errors.New("application source HTTPS probe did not complete")
	}
	// HEAD response bodies are never captured or transported as evidence.
	closeErr := response.Body.Close()
	if closeErr != nil || ctx.Err() != nil || response.TLS == nil || len(response.TLS.VerifiedChains) == 0 ||
		response.TLS.ServerName != input.SourceHost || response.StatusCode < 200 || response.StatusCode > 399 {
		return 0, errors.New("application source relay did not provide verified public HTTPS")
	}
	return response.StatusCode, nil
}
