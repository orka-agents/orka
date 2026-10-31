package wire_test

import (
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/resolver"
	"google.golang.org/grpc/resolver/manual"

	storagev1 "github.com/orka-agents/orka/pkg/storage/v1"
	"github.com/orka-agents/orka/pkg/storage/v1/wire"
)

// These tests check grpc-go's behavior that send evidence depends on, with
// faults injected below the gRPC layer. They use plaintext TCP because the
// faults are written as raw HTTP/2.

// A refused dial never creates a stream, so even a write is safely
// unavailable.
func TestSendEvidenceRefusedDialIsNotSent(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	address := listener.Addr().String()
	require.NoError(t, listener.Close())

	client := wire.NewClient(dialInsecure(t, address), wire.ClientConfig{})
	err = client.SavePlan(t.Context(), "n", "t", &storagev1.PlanState{})
	require.ErrorIs(t, err, storagev1.ErrUnavailable)
	require.False(t, errors.Is(err, storagev1.ErrOutcomeUnknown), "nothing was sent: %v", err)
}

// A write that fails before grpc-go starts any attempt sent nothing, on a
// connection built with Dial.
func TestSendEvidenceFailuresBeforeAnyAttemptAreNotSent(t *testing.T) {
	t.Run("closed connection", func(t *testing.T) {
		conn := dialInsecure(t, "127.0.0.1:1")
		require.NoError(t, conn.Close())
		err := wire.NewClient(conn, wire.ClientConfig{}).SavePlan(t.Context(), "n", "t", &storagev1.PlanState{})
		require.ErrorIs(t, err, storagev1.ErrUnavailable)
		require.False(t, errors.Is(err, storagev1.ErrOutcomeUnknown), "nothing was sent: %v", err)
	})
	t.Run("deadline while waiting for name resolution", func(t *testing.T) {
		// The manual resolver never reports an address, so the call waits for
		// resolution until its deadline. Dial takes no resolver option, so the
		// test registers the scheme globally.
		resolver.Register(manual.NewBuilderWithScheme("wire-evidence-never"))
		conn, err := wire.Dial("wire-evidence-never:///storage", wire.DialConfig{TransportCredentials: insecure.NewCredentials()})
		require.NoError(t, err)
		t.Cleanup(func() { _ = conn.Close() })
		ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
		defer cancel()
		err = wire.NewClient(conn, wire.ClientConfig{}).SavePlan(ctx, "n", "t", &storagev1.PlanState{})
		require.ErrorIs(t, err, storagev1.ErrUnavailable)
		require.ErrorIs(t, err, context.DeadlineExceeded)
		require.False(t, errors.Is(err, storagev1.ErrOutcomeUnknown), "nothing was sent: %v", err)
	})
	for name, opts := range map[string][]grpc.DialOption{
		"connection built with DialOptions alone": append(wire.DialOptions(), grpc.WithTransportCredentials(insecure.NewCredentials())),
		"connection without the evidence handler": {grpc.WithTransportCredentials(insecure.NewCredentials())},
	} {
		t.Run(name, func(t *testing.T) {
			conn, err := grpc.NewClient("passthrough:///127.0.0.1:1", opts...)
			require.NoError(t, err)
			require.NoError(t, conn.Close())
			err = wire.NewClient(conn, wire.ClientConfig{}).SavePlan(t.Context(), "n", "t", &storagev1.PlanState{})
			require.ErrorIs(t, err, storagev1.ErrOutcomeUnknown,
				"without Dial's coverage, a write's transport failure stays an unknown outcome")
		})
	}
	t.Run("dial without transport credentials", func(t *testing.T) {
		_, err := wire.Dial("passthrough:///127.0.0.1:1", wire.DialConfig{})
		require.Error(t, err)
	})
}

// slowFirstDial delays the first connection, so a call can start an attempt
// and fail on its deadline before any stream exists.
type slowFirstDial struct {
	once  sync.Once
	delay time.Duration
}

func (d *slowFirstDial) dial(ctx context.Context, address string) (net.Conn, error) {
	d.once.Do(func() { time.Sleep(d.delay) })
	return (&net.Dialer{}).DialContext(ctx, "tcp", address)
}

// firstThenDetached is a caller interceptor that first tries the call on its
// own context with a short deadline, which fails before a stream exists, and
// then sends it again on a fresh context that carries only its metadata.
func firstThenDetached(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn,
	invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
	short, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
	defer cancel()
	if err := invoker(short, method, req, reply, cc, opts...); err == nil {
		return nil
	}
	header, _ := metadata.FromOutgoingContext(ctx)
	return invoker(metadata.NewOutgoingContext(context.Background(), header), method, req, reply, cc, opts...)
}

// A caller interceptor can retry a call where its send evidence cannot see
// it. On a connection that admits caller interceptors, a write that reached
// the provider must stay an unknown outcome, even when the evidence it can
// see shows only an attempt that sent nothing.
func TestSendEvidenceIsConservativeWithCallerInterceptors(t *testing.T) {
	for _, wrapped := range []bool{false, true} {
		name := "interceptor on a DialOptions connection"
		if wrapped {
			name = "wrapper around a Dial connection"
		}
		t.Run(name, func(t *testing.T) {
			testRetriedWriteStaysUnknown(t, wrapped)
		})
	}
}

// retryingConn wraps a connection and retries each call the way
// firstThenDetached does, outside the connection's own interceptors.
type retryingConn struct {
	inner *grpc.ClientConn
}

func (c retryingConn) Invoke(ctx context.Context, method string, args, reply any, opts ...grpc.CallOption) error {
	short, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
	defer cancel()
	if err := c.inner.Invoke(short, method, args, reply, opts...); err == nil {
		return nil
	}
	header, _ := metadata.FromOutgoingContext(ctx)
	return c.inner.Invoke(metadata.NewOutgoingContext(context.Background(), header), method, args, reply, opts...)
}

func (c retryingConn) NewStream(ctx context.Context, desc *grpc.StreamDesc, method string, opts ...grpc.CallOption) (grpc.ClientStream, error) {
	return c.inner.NewStream(ctx, desc, method, opts...)
}

func testRetriedWriteStaysUnknown(t *testing.T, wrapped bool) {
	provider := newRecordingProvider()
	release := make(chan struct{})
	provider.block["SavePlan"] = release
	server, err := wire.NewServer(wire.ServerConfig{State: provider})
	require.NoError(t, err)
	tcp, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	listener := &trackingListener{Listener: tcp}
	grpcServer := grpc.NewServer(server.ServerOptions()...)
	server.Register(grpcServer)
	go func() { _ = grpcServer.Serve(listener) }()
	t.Cleanup(grpcServer.Stop)
	dialer := &slowFirstDial{delay: 200 * time.Millisecond}
	var client *wire.Client
	if wrapped {
		conn, err := wire.Dial("passthrough:///"+tcp.Addr().String(), wire.DialConfig{
			TransportCredentials: insecure.NewCredentials(),
			ContextDialer:        dialer.dial,
		})
		require.NoError(t, err)
		t.Cleanup(func() { _ = conn.Close() })
		client = wire.NewClient(retryingConn{inner: conn}, wire.ClientConfig{})
	} else {
		conn, err := grpc.NewClient("passthrough:///"+tcp.Addr().String(), append(wire.DialOptions(),
			grpc.WithTransportCredentials(insecure.NewCredentials()),
			grpc.WithContextDialer(dialer.dial),
			grpc.WithChainUnaryInterceptor(firstThenDetached))...)
		require.NoError(t, err)
		t.Cleanup(func() { _ = conn.Close() })
		client = wire.NewClient(conn, wire.ClientConfig{})
	}

	result := make(chan error, 1)
	go func() { result <- client.SavePlan(t.Context(), "n", "t", &storagev1.PlanState{}) }()
	select {
	case <-provider.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the call never reached the provider")
	}
	listener.resetAll()
	select {
	case err = <-result:
	case <-time.After(10 * time.Second):
		t.Fatal("the call did not fail after the reset")
	}
	close(release)
	require.ErrorIs(t, err, storagev1.ErrOutcomeUnknown)
}

// trackingListener remembers accepted connections so a test can reset them.
type trackingListener struct {
	net.Listener
	mu    sync.Mutex
	conns []net.Conn
}

func (l *trackingListener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err == nil {
		l.mu.Lock()
		l.conns = append(l.conns, conn)
		l.mu.Unlock()
	}
	return conn, err
}

func (l *trackingListener) resetAll() {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, conn := range l.conns {
		if tcp, ok := conn.(*net.TCPConn); ok {
			_ = tcp.SetLinger(0)
		}
		_ = conn.Close()
	}
}

// A connection reset after the server received a write's headers may follow
// a commit, so the write's outcome is unknown; the same failure on a read is
// safe to repeat.
func TestSendEvidenceResetAfterTheServerReceivedTheCall(t *testing.T) {
	for _, call := range []struct {
		method  string
		invoke  func(*wire.Client) error
		unknown bool
	}{
		{"SavePlan", func(c *wire.Client) error { return c.SavePlan(t.Context(), "n", "t", &storagev1.PlanState{}) }, true},
		{"GetPlan", func(c *wire.Client) error { _, err := c.GetPlan(t.Context(), "n", "t"); return err }, false},
	} {
		t.Run(call.method, func(t *testing.T) {
			provider := newRecordingProvider()
			release := make(chan struct{})
			provider.block[call.method] = release
			server, err := wire.NewServer(wire.ServerConfig{State: provider})
			require.NoError(t, err)
			tcp, err := net.Listen("tcp", "127.0.0.1:0")
			require.NoError(t, err)
			listener := &trackingListener{Listener: tcp}
			grpcServer := grpc.NewServer(server.ServerOptions()...)
			server.Register(grpcServer)
			go func() { _ = grpcServer.Serve(listener) }()
			t.Cleanup(grpcServer.Stop)
			client := wire.NewClient(dialInsecure(t, tcp.Addr().String()), wire.ClientConfig{})

			result := make(chan error, 1)
			go func() { result <- call.invoke(client) }()
			select {
			case <-provider.entered:
			case <-time.After(10 * time.Second):
				t.Fatal("the call never reached the provider")
			}
			listener.resetAll()
			var err2 error
			select {
			case err2 = <-result:
			case <-time.After(10 * time.Second):
				t.Fatal("the call did not fail after the reset")
			}
			close(release)
			if call.unknown {
				require.ErrorIs(t, err2, storagev1.ErrOutcomeUnknown)
			} else {
				require.ErrorIs(t, err2, storagev1.ErrUnavailable)
				require.False(t, errors.Is(err2, storagev1.ErrOutcomeUnknown))
			}
		})
	}
}

// goAwayListener answers its first connection with a raw HTTP/2 server that
// sends GOAWAY (last stream 0) as soon as the call's headers arrive, so the
// server never processes the stream. Later connections go to the real
// server.
type goAwayListener struct {
	net.Listener
	once       sync.Once
	sawHeaders chan struct{}
}

func (l *goAwayListener) Accept() (net.Conn, error) {
	for {
		conn, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		first := false
		l.once.Do(func() { first = true })
		if !first {
			return conn, nil
		}
		go l.goAwayOnFirstStream(conn)
	}
}

func (l *goAwayListener) goAwayOnFirstStream(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	preface := make([]byte, len("PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n"))
	if _, err := io.ReadFull(conn, preface); err != nil {
		return
	}
	// An empty SETTINGS frame is the server preface.
	if _, err := conn.Write([]byte{0, 0, 0, 0x4, 0, 0, 0, 0, 0}); err != nil {
		return
	}
	header := make([]byte, 9)
	for {
		if _, err := io.ReadFull(conn, header); err != nil {
			return
		}
		length := int(header[0])<<16 | int(header[1])<<8 | int(header[2])
		if _, err := io.CopyN(io.Discard, conn, int64(length)); err != nil {
			return
		}
		switch frameType, flags := header[3], header[4]; {
		case frameType == 0x4 && flags&0x1 == 0:
			_, _ = conn.Write([]byte{0, 0, 0, 0x4, 0x1, 0, 0, 0, 0}) // SETTINGS ACK
		case frameType == 0x1:
			// GOAWAY: last processed stream 0, NO_ERROR.
			_, _ = conn.Write([]byte{0, 0, 8, 0x7, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0})
			close(l.sawHeaders)
			_, _ = io.Copy(io.Discard, conn)
			return
		}
	}
}

// A GOAWAY that excludes a stream means the server never processed it, so
// grpc-go retries it transparently on a new connection, below send evidence
// and without storage configuring any retry policy.
func TestSendEvidenceGoAwayBeforeProcessingIsRetriedTransparently(t *testing.T) {
	provider := newRecordingProvider()
	server, err := wire.NewServer(wire.ServerConfig{State: provider})
	require.NoError(t, err)
	tcp, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	listener := &goAwayListener{Listener: tcp, sawHeaders: make(chan struct{})}
	grpcServer := grpc.NewServer(server.ServerOptions()...)
	server.Register(grpcServer)
	go func() { _ = grpcServer.Serve(listener) }()
	t.Cleanup(grpcServer.Stop)
	client := wire.NewClient(dialInsecure(t, tcp.Addr().String()), wire.ClientConfig{})

	require.NoError(t, client.SavePlan(t.Context(), "n", "t", &storagev1.PlanState{Summary: "once"}))
	select {
	case <-listener.sawHeaders:
	default:
		t.Fatal("the first connection never received the call's headers")
	}
	require.Len(t, provider.callsTo("SavePlan"), 1, "the provider ran the write exactly once")
}
