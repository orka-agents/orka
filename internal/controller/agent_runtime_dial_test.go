package controller

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"net"
	"net/http"
	"strings"
	"testing"
)

func TestAgentRuntimeV1DialControlledClientNonServiceRejectsPrivate(t *testing.T) {
	roots := x509.NewCertPool()
	base := &http.Client{Transport: &http.Transport{
		TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots},
		MaxIdleConns:    5,
	}}

	client := agentRuntimeV1DialControlledClient(base, "https://runtime.example.com:8443", nil)
	transport, ok := client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("controlled transport type = %T", client.Transport)
	}
	// The configured TLS roots and other settings must survive the clone so v1
	// readiness probes keep trusting the operator-provided CA.
	if transport.TLSClientConfig == nil || transport.TLSClientConfig.RootCAs != roots {
		t.Fatal("configured TLS roots were not preserved")
	}
	if transport.MaxIdleConns != 5 {
		t.Fatalf("transport settings lost during clone: %d", transport.MaxIdleConns)
	}
	if _, err := transport.DialContext(context.Background(), "tcp", "10.0.0.5:8443"); err == nil ||
		!strings.Contains(err.Error(), "not a public address") {
		t.Fatalf("non-Service dial control err = %v", err)
	}
	// The shared base client must not have been mutated by the clone.
	if base.Transport.(*http.Transport).DialContext != nil {
		t.Fatal("base client transport was mutated")
	}
}

func TestAgentRuntimeV1DialControlledClientServicePinsBackends(t *testing.T) {
	roots := x509.NewCertPool()
	base := &http.Client{Transport: &http.Transport{
		TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots},
	}}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close() //nolint:errcheck
	go func() {
		conn, acceptErr := listener.Accept()
		if acceptErr == nil {
			_ = conn.Close()
		}
	}()

	client := agentRuntimeV1DialControlledClient(
		base, "https://runtime.orka-runtimes.svc.cluster.local:8443", []string{listener.Addr().String()})
	transport := client.Transport.(*http.Transport)
	if transport.TLSClientConfig == nil || transport.TLSClientConfig.RootCAs != roots {
		t.Fatal("pinned transport dropped the configured TLS roots")
	}
	// A Service endpoint would resolve to a mutable ClusterIP; the pinned dial
	// must reach the verified backend regardless of the requested address.
	conn, err := transport.DialContext(context.Background(), "tcp", "10.96.0.10:8443")
	if err != nil {
		t.Fatalf("pinned dial: %v", err)
	}
	defer conn.Close() //nolint:errcheck
	if got := conn.RemoteAddr().String(); got != listener.Addr().String() {
		t.Fatalf("pinned dial reached %s, want %s", got, listener.Addr().String())
	}
}
