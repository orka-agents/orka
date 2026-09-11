package conformance_test

import (
	"context"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/orka-agents/orka/internal/harness/v2/conformance"
)

func TestPinnedBackendDialTransportDialsPinsOnly(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close() //nolint:errcheck
	accepted := make(chan struct{}, 1)
	go func() {
		conn, acceptErr := listener.Accept()
		if acceptErr != nil {
			return
		}
		accepted <- struct{}{}
		_ = conn.Close()
	}()
	transport := conformance.PinnedBackendDialTransport([]string{listener.Addr().String()})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	// The requested address is deliberately not the pinned backend: a Service
	// ClusterIP would resolve here, and the transport must ignore it in favor of
	// the verified backend.
	conn, err := transport.DialContext(ctx, "tcp", "203.0.113.7:9999")
	if err != nil {
		t.Fatalf("pinned dial: %v", err)
	}
	defer conn.Close() //nolint:errcheck
	if got := conn.RemoteAddr().String(); got != listener.Addr().String() {
		t.Fatalf("pinned dial reached %s, want %s", got, listener.Addr().String())
	}
	select {
	case <-accepted:
	case <-time.After(time.Second):
		t.Fatal("pinned listener did not accept")
	}
}

func TestPinnedBackendDialTransportFailsClosedWithoutPins(t *testing.T) {
	transport := conformance.PinnedBackendDialTransport(nil)
	_, err := transport.DialContext(context.Background(), "tcp", "203.0.113.7:9999")
	if err == nil || !strings.Contains(err.Error(), "no verified backend") {
		t.Fatalf("empty-pin dial err = %v", err)
	}
}

func TestPublicAddressDialTransportRejectsNonPublicAddress(t *testing.T) {
	transport := conformance.PublicAddressDialTransport()
	for _, address := range []string{"10.0.0.5:443", "127.0.0.1:443", "169.254.0.1:443", "100.64.0.1:443"} {
		if _, err := transport.DialContext(context.Background(), "tcp", address); err == nil ||
			!strings.Contains(err.Error(), "not a public address") {
			t.Fatalf("dial %s err = %v", address, err)
		}
	}
}

func TestApplyPublicAddressDialControlPreservesTransport(t *testing.T) {
	transport := &http.Transport{}
	transport.MaxIdleConns = 7
	conformance.ApplyPublicAddressDialControl(transport)
	if transport.MaxIdleConns != 7 {
		t.Fatalf("ApplyPublicAddressDialControl clobbered transport settings: %d", transport.MaxIdleConns)
	}
	if transport.DialContext == nil {
		t.Fatal("ApplyPublicAddressDialControl did not install a dial control")
	}
	if _, err := transport.DialContext(context.Background(), "tcp", "192.168.1.10:443"); err == nil ||
		!strings.Contains(err.Error(), "not a public address") {
		t.Fatalf("controlled dial err = %v", err)
	}
}
