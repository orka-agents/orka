//go:build !windows

package main

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/intstr"
	portprotocol "k8s.io/apimachinery/pkg/util/portforward"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/portforward"
	"k8s.io/client-go/transport/websocket"
	"k8s.io/streaming/pkg/httpstream"
	streamspdy "k8s.io/streaming/pkg/httpstream/spdy"
)

const migrationTunnelHost = "orka-migration.invalid"

type migrationRequestContextKey struct{}

type migrationTransport struct{ *http.Transport }

func (t *migrationTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	// net/http detaches cancellation from DialContext so pooled dials can
	// outlive their caller. Migration uses one tunnel per request instead.
	ctx := context.WithValue(request.Context(), migrationRequestContextKey{}, request.Context())
	return t.Transport.RoundTrip(request.Clone(ctx))
}

// The HTTP socket is an in-process pipe to authenticated Kubernetes streams.
// No localhost listener, subprocess, ambient HTTP proxy, or cached tunnel can
// receive migration credentials or portable conversation data.
func newMigrationHTTPClient(ctx context.Context, config *rest.Config, kube kubernetes.Interface, service *corev1.Service) (*http.Client, func()) {
	lifetime, cancel := context.WithCancel(ctx)
	transport := &http.Transport{
		DisableKeepAlives: true,
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			if network != "tcp" || address != migrationTunnelHost+":80" {
				return nil, errors.New("invalid native migration tunnel destination")
			}
			requestContext, ok := ctx.Value(migrationRequestContextKey{}).(context.Context)
			if !ok {
				return nil, errors.New("missing native migration request context")
			}
			dialCtx, dialCancel := context.WithTimeout(requestContext, 30*time.Second)
			stop := context.AfterFunc(lifetime, dialCancel)
			defer stop()
			defer dialCancel()
			pod, port, err := migrationServicePod(dialCtx, kube, service)
			if err != nil {
				return nil, errors.New("resolve native migration service backend")
			}
			url := kube.CoreV1().RESTClient().Post().Namespace(service.Namespace).Resource("pods").Name(pod).SubResource("portforward").URL()
			stream, err := dialMigrationStream(dialCtx, config, url.String())
			if err != nil {
				// Upstream errors can contain response bodies or authentication
				// metadata. Neither belongs in migration command output.
				return nil, errors.New("connect native migration service")
			}
			return migrationPipe(dialCtx, lifetime, stream, port)
		},
	}
	return &http.Client{
		Transport: &migrationTransport{transport},
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}, func() {
		cancel()
		transport.CloseIdleConnections()
	}
}

// Match kubectl's service port-forward semantics, but retain the exact Service
// identity and selector chosen for this migration rather than a local port.
func migrationServicePod(ctx context.Context, kube kubernetes.Interface, expected *corev1.Service) (string, int32, error) {
	service, err := kube.CoreV1().Services(expected.Namespace).Get(ctx, expected.Name, metav1.GetOptions{})
	if err != nil || service.UID != expected.UID || len(service.Spec.Selector) == 0 || service.Spec.Type == corev1.ServiceTypeExternalName {
		return "", 0, errors.New("invalid migration service")
	}
	var selected *corev1.ServicePort
	for i := range service.Spec.Ports {
		port := &service.Spec.Ports[i]
		if port.Port == 8080 && port.Protocol == corev1.ProtocolTCP {
			selected = port
			break
		}
	}
	if selected == nil {
		return "", 0, errors.New("migration service has no TCP port 8080")
	}
	pods, err := kube.CoreV1().Pods(service.Namespace).List(ctx, metav1.ListOptions{LabelSelector: labels.SelectorFromSet(service.Spec.Selector).String()})
	if err != nil {
		return "", 0, err
	}
	for _, pod := range pods.Items {
		if pod.DeletionTimestamp != nil || pod.Status.Phase != corev1.PodRunning || !labels.SelectorFromSet(service.Spec.Selector).Matches(labels.Set(pod.Labels)) {
			continue
		}
		ready := false
		for _, condition := range pod.Status.Conditions {
			ready = ready || condition.Type == corev1.PodReady && condition.Status == corev1.ConditionTrue
		}
		if !ready {
			continue
		}
		port := selected.TargetPort.IntVal
		if selected.TargetPort.Type == intstr.String {
			for _, container := range pod.Spec.Containers {
				for _, candidate := range container.Ports {
					if candidate.Name == selected.TargetPort.StrVal && candidate.Protocol == corev1.ProtocolTCP {
						port = candidate.ContainerPort
					}
				}
			}
		}
		if port > 0 && port <= 65535 {
			return pod.Name, port, nil
		}
	}
	return "", 0, errors.New("migration service has no ready backend")
}

func dialMigrationStream(ctx context.Context, config *rest.Config, endpoint string) (httpstream.Connection, error) {
	// Prefer the same WebSocket-wrapped SPDY protocol as current kubectl. It
	// supports HTTP proxies that cannot pass a traditional SPDY upgrade.
	var stopSocket func() bool
	trace := &httptrace.ClientTrace{GotConn: func(info httptrace.GotConnInfo) {
		stopSocket = context.AfterFunc(ctx, func() { _ = info.Conn.Close() })
	}}
	defer func() {
		if stopSocket != nil {
			stopSocket()
		}
	}()
	req, err := http.NewRequestWithContext(httptrace.WithClientTrace(ctx, trace), http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	transport, holder, err := websocket.RoundTripperFor(config)
	if err != nil {
		return nil, err
	}
	ws, err := websocket.Negotiate(transport, holder, req, portprotocol.WebsocketsSPDYTunnelingPortForwardV1)
	if err == nil {
		if ws.Subprotocol() != portprotocol.WebsocketsSPDYTunnelingPortForwardV1 {
			_ = ws.Close()
			return nil, errors.New("unexpected native migration tunnel protocol")
		}
		tunnel := portforward.NewTunnelingConnectionWithLogger(logr.Discard(), ws)
		conn, err := streamspdy.NewClientConnectionWithPings(tunnel, portforward.PingPeriod)
		if err != nil {
			_ = ws.Close()
			return nil, err
		}
		return newMigrationStream(conn, ws), nil
	}
	if !httpstream.IsUpgradeFailure(err) && !httpstream.IsHTTPSProxyError(err) {
		return nil, err
	}
	if stopSocket != nil {
		stopSocket()
	}
	return dialMigrationSPDY(ctx, config, endpoint)
}

// Use net/http's context-aware upgrade instead of client-go's SPDY upgrade
// transport, which can block in ReadResponse after its dial context expires.
func dialMigrationSPDY(ctx context.Context, config *rest.Config, endpoint string) (httpstream.Connection, error) {
	config = rest.CopyConfig(config)
	config.NextProtos = []string{"http/1.1"}
	transport, err := rest.TransportFor(config)
	if err != nil {
		return nil, err
	}
	var socket net.Conn
	trace := &httptrace.ClientTrace{GotConn: func(info httptrace.GotConnInfo) { socket = info.Conn }}
	req, err := http.NewRequestWithContext(httptrace.WithClientTrace(ctx, trace), http.MethodPost, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set(httpstream.HeaderConnection, httpstream.HeaderUpgrade)
	req.Header.Set(httpstream.HeaderUpgrade, streamspdy.HeaderSpdy31)
	req.Header.Set(httpstream.HeaderProtocolVersion, portforward.PortForwardProtocolV1Name)
	client := &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Do(req)
	if err != nil {
		if socket != nil {
			_ = socket.Close()
		}
		return nil, err
	}
	rw, writable := response.Body.(io.ReadWriteCloser)
	if response.StatusCode != http.StatusSwitchingProtocols || !writable || socket == nil ||
		!strings.EqualFold(response.Header.Get(httpstream.HeaderUpgrade), streamspdy.HeaderSpdy31) ||
		response.Header.Get(httpstream.HeaderProtocolVersion) != portforward.PortForwardProtocolV1Name {
		if socket != nil {
			_ = socket.Close()
		}
		_ = response.Body.Close()
		return nil, errors.New("native migration SPDY upgrade rejected")
	}
	// The response reader may have buffered the first SPDY frame. Preserve it
	// while retaining the physical socket for unconditional cancellation.
	conn := &migrationUpgradeConn{Conn: socket, readerWriter: rw}
	stream, err := streamspdy.NewClientConnectionWithPings(conn, portforward.PingPeriod)
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	return newMigrationStream(stream, conn), nil
}

type migrationUpgradeConn struct {
	net.Conn
	readerWriter io.ReadWriteCloser
}

func (c *migrationUpgradeConn) Read(p []byte) (int, error)  { return c.readerWriter.Read(p) }
func (c *migrationUpgradeConn) Write(p []byte) (int, error) { return c.readerWriter.Write(p) }

// Force-close the socket before graceful SPDY cleanup. An unacknowledged
// stream is not in client-go's reset list and can otherwise hold shutdown open.
type migrationStream struct {
	httpstream.Connection
	socket io.Closer
	once   sync.Once
	closed chan bool
}

func newMigrationStream(stream httpstream.Connection, socket io.Closer) *migrationStream {
	conn := &migrationStream{Connection: stream, socket: socket, closed: make(chan bool)}
	go func() {
		select {
		case <-stream.CloseChan():
			_ = conn.Close()
		case <-conn.closed:
		}
	}()
	return conn
}

func (c *migrationStream) Close() error {
	c.once.Do(func() {
		_ = c.socket.Close()
		close(c.closed)
		_ = c.Connection.Close()
	})
	return nil
}

func (c *migrationStream) CloseChan() <-chan bool { return c.closed }

func (c *migrationStream) CreateStream(headers http.Header) (httpstream.Stream, error) {
	type result struct {
		stream httpstream.Stream
		err    error
	}
	ready := make(chan result, 1)
	go func() {
		stream, err := c.Connection.CreateStream(headers)
		if stream != nil {
			select {
			case <-c.closed:
				_ = stream.Reset()
				c.RemoveStreams(stream)
			default:
			}
		}
		ready <- result{stream, err}
	}()
	// The dependency's acknowledgment wait has a fixed 30-second bound and
	// ignores socket EOF. Return promptly on closure; its worker can finish
	// that bounded wait without retaining an open socket or blocking cleanup.
	select {
	case <-c.closed:
		return nil, errors.New("native migration tunnel closed")
	case result := <-ready:
		return result.stream, result.err
	}
}

func migrationPipe(setup, lifetime context.Context, stream httpstream.Connection, port int32) (net.Conn, error) {
	stopSetup := context.AfterFunc(setup, func() { _ = stream.Close() })
	defer stopSetup()
	stop := context.AfterFunc(lifetime, func() { _ = stream.Close() })
	headers := http.Header{}
	headers.Set(corev1.StreamType, corev1.StreamTypeError)
	headers.Set(corev1.PortHeader, strconv.Itoa(int(port)))
	headers.Set(corev1.PortForwardRequestIDHeader, "0")
	errorStream, err := stream.CreateStream(headers)
	if err != nil {
		stop()
		_ = stream.Close()
		return nil, errors.New("create native migration error stream")
	}
	_ = errorStream.Close() // We only read the server's error channel.
	headers.Set(corev1.StreamType, corev1.StreamTypeData)
	dataStream, err := stream.CreateStream(headers)
	if err != nil {
		stop()
		_ = stream.Close()
		return nil, errors.New("create native migration data stream")
	}
	if err := setup.Err(); err != nil {
		stop()
		_ = stream.Close()
		return nil, err
	}
	local, remote := net.Pipe()
	var once sync.Once
	closeTunnel := func() {
		once.Do(func() {
			stop()
			_ = local.Close()
			_ = remote.Close()
			_ = stream.Close()
		})
	}
	go func() {
		defer closeTunnel()
		_, _ = io.Copy(remote, dataStream)
	}()
	go func() {
		defer closeTunnel()
		_, _ = io.Copy(dataStream, remote)
	}()
	go func() {
		// A forwarding failure closes the pipe without exposing server text.
		var message [1]byte
		n, err := errorStream.Read(message[:])
		if n > 0 || err != nil && !errors.Is(err, io.EOF) {
			closeTunnel()
		}
	}()
	return local, nil
}
