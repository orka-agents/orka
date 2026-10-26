package supervisor

import (
	"context"
	"net"
	"testing"
	"time"

	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
	"golang.org/x/sys/unix"
)

func requirePlainTCP(t *testing.T, listener net.Listener) {
	t.Helper()
	tcp, ok := listener.(*net.TCPListener)
	if !ok {
		t.Fatalf("listener has type %T, want TCP", listener)
	}
	raw, err := tcp.SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	var protocol int
	var socketErr error
	if err := raw.Control(func(fd uintptr) {
		protocol, socketErr = unix.GetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_PROTOCOL)
	}); err != nil {
		t.Fatal(err)
	}
	if socketErr != nil {
		t.Fatal(socketErr)
	}
	if protocol != unix.IPPROTO_TCP {
		t.Fatalf("listener protocol = %d, want ordinary TCP (%d)", protocol, unix.IPPROTO_TCP)
	}
}

func TestSupervisorListenersUsePlainTCP(t *testing.T) {
	for _, setting := range []string{"", "multipathtcp=1"} {
		t.Run("GODEBUG="+setting, func(t *testing.T) {
			t.Setenv("GODEBUG", setting)
			t.Run("control", func(t *testing.T) {
				listener, err := ListenTCP("tcp", "127.0.0.1:0")
				if err != nil {
					t.Fatal(err)
				}
				defer listener.Close()
				requirePlainTCP(t, listener)
			})
			t.Run("provider", func(t *testing.T) {
				proxy := newTestProviderProxy(t, ProviderProxyConfig{
					UpstreamBaseURL: testUnreachableUpstreamURL, UpstreamBearerToken: testUpstreamToken,
				})
				requirePlainTCP(t, proxy.listener)
				if !proxy.listener.Addr().(*net.TCPAddr).IP.Equal(net.IPv4(127, 0, 0, 1)) {
					t.Fatal("provider listener escaped IPv4 loopback")
				}
			})
			t.Run("mcp", func(t *testing.T) {
				proxy, err := newMCPProxy(MCPBrokerFunc(func(context.Context, harnessv2.MCPBrokerCallRequest) (harnessv2.MCPBrokerCallResponse, error) {
					t.Error("listener qualification must not call the broker")
					return harnessv2.MCPBrokerCallResponse{}, nil
				}))
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
					defer cancel()
					if err := proxy.close(ctx); err != nil {
						t.Errorf("close MCP proxy: %v", err)
					}
				})
				requirePlainTCP(t, proxy.listener)
				if !proxy.listener.Addr().(*net.TCPAddr).IP.Equal(net.IPv4(127, 0, 0, 1)) {
					t.Fatal("MCP listener escaped IPv4 loopback")
				}
			})
		})
	}
}
