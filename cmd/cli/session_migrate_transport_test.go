//go:build !windows

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr"
	gwebsocket "github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	portprotocol "k8s.io/apimachinery/pkg/util/portforward"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/portforward"
	"k8s.io/streaming/pkg/httpstream"
	streamspdy "k8s.io/streaming/pkg/httpstream/spdy"
)

type migrationKubeFixture struct {
	t           *testing.T
	mu          sync.Mutex
	serviceUID  types.UID
	target      string
	websocket   bool
	tunnels     int
	closed      int
	failForward bool
}

func newMigrationKubeFixture(t *testing.T, target string) *migrationKubeFixture {
	t.Helper()
	return &migrationKubeFixture{t: t, target: target, serviceUID: "stable-service-uid", websocket: true}
}

func (f *migrationKubeFixture) setServiceUID(uid types.UID) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.serviceUID = uid
}
func (f *migrationKubeFixture) tunnelCount() int { f.mu.Lock(); defer f.mu.Unlock(); return f.tunnels }
func (f *migrationKubeFixture) closedCount() int { f.mu.Lock(); defer f.mu.Unlock(); return f.closed }
func (f *migrationKubeFixture) service() *corev1.Service {
	f.mu.Lock()
	defer f.mu.Unlock()
	return &corev1.Service{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Service"},
		ObjectMeta: metav1.ObjectMeta{Namespace: "test", Name: "orka-api", UID: f.serviceUID},
		Spec:       corev1.ServiceSpec{Selector: map[string]string{"app": "orka-api"}, Ports: []corev1.ServicePort{{Port: 8080, Protocol: corev1.ProtocolTCP, TargetPort: intstr.FromString("api")}}},
	}
}

func migrationReadyPod() corev1.Pod {
	return corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "orka-backend", Namespace: "test", Labels: map[string]string{"app": "orka-api"}},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "api", Ports: []corev1.ContainerPort{{Name: "api", ContainerPort: 9090, Protocol: corev1.ProtocolTCP}}}}},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning, Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}},
	}
}

func (f *migrationKubeFixture) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer test-kube-only" || r.Header.Get("Txn-Token") != "" {
		f.t.Error("Kubernetes request used unexpected credentials")
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	switch r.URL.Path {
	case "/api/v1/namespaces/test/services/orka-api":
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(f.service())
	case "/api/v1/namespaces/test/pods":
		if r.URL.Query().Get("labelSelector") != "app=orka-api" {
			f.t.Error("wrong backend selector")
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(corev1.PodList{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "PodList"}, Items: []corev1.Pod{migrationReadyPod()}})
	case "/api/v1/namespaces/test/pods/orka-backend/portforward":
		f.forward(w, r)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func (f *migrationKubeFixture) forward(w http.ResponseWriter, r *http.Request) {
	streams := make(chan httpstream.Stream, 2)
	handleStream := func(stream httpstream.Stream, reply <-chan struct{}) error {
		if stream.Headers().Get(corev1.PortHeader) != "9090" || stream.Headers().Get(corev1.PortForwardRequestIDHeader) != "0" {
			return errors.New("invalid forwarding target")
		}
		go func() { <-reply; streams <- stream }()
		return nil
	}
	var conn httpstream.Connection
	if r.Method == http.MethodGet && f.websocket {
		upgrader := gwebsocket.Upgrader{Subprotocols: []string{portprotocol.WebsocketsSPDYTunnelingPortForwardV1}}
		ws, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			f.t.Error(err)
			return
		}
		tunnel := portforward.NewTunnelingConnectionWithLogger(logr.Discard(), ws)
		conn, err = streamspdy.NewServerConnection(tunnel, handleStream)
		if err != nil {
			f.t.Error(err)
			_ = tunnel.Close()
			return
		}
	} else if r.Method == http.MethodPost {
		w.Header().Set(httpstream.HeaderProtocolVersion, portforward.PortForwardProtocolV1Name)
		conn = streamspdy.NewResponseUpgrader().UpgradeResponse(w, r, handleStream)
	} else {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	if conn == nil {
		return
	}
	f.mu.Lock()
	f.tunnels++
	f.mu.Unlock()
	defer func() { _ = conn.Close(); f.mu.Lock(); f.closed++; f.mu.Unlock() }()
	var errorStream, dataStream httpstream.Stream
	for range 2 {
		select {
		case stream := <-streams:
			if stream.Headers().Get(corev1.StreamType) == corev1.StreamTypeError {
				errorStream = stream
			} else {
				dataStream = stream
			}
		case <-conn.CloseChan():
			return
		}
	}
	if errorStream == nil || dataStream == nil {
		f.t.Error("missing port-forward streams")
		return
	}
	if f.failForward {
		_, _ = errorStream.Write([]byte("private server diagnostic"))
		<-conn.CloseChan()
		return
	}
	target, err := url.Parse(f.target)
	if err != nil {
		f.t.Error(err)
		return
	}
	upstream, err := net.DialTimeout("tcp", target.Host, time.Second)
	if err != nil {
		f.t.Error(err)
		return
	}
	go func() { <-conn.CloseChan(); _ = upstream.Close() }()
	defer func() { _ = upstream.Close() }()
	go func() { _, _ = io.Copy(upstream, dataStream); _ = upstream.Close() }()
	_, _ = io.Copy(dataStream, upstream)
	_ = dataStream.Close()
	_ = errorStream.Close()
	<-conn.CloseChan()
}

func TestMigrationHTTPClientVerifiedTunnel(t *testing.T) {
	for _, websocket := range []bool{true, false} {
		name := "spdy"
		if websocket {
			name = "websocket"
		}
		t.Run(name, func(t *testing.T) {
			payload := bytes.Repeat([]byte("private migration bundle\n"), 140000)
			api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				require.Equal(t, migrationTunnelHost, r.Host)
				require.Equal(t, "Bearer test-orka-only", r.Header.Get("Authorization"))
				require.Equal(t, "test-transaction-only", r.Header.Get("Txn-Token"))
				body, err := io.ReadAll(r.Body)
				require.NoError(t, err)
				require.Equal(t, payload, body)
				_, _ = w.Write(payload)
			}))
			defer api.Close()
			fixture := newMigrationKubeFixture(t, api.URL)
			fixture.websocket = websocket
			kube := httptest.NewTLSServer(fixture)
			defer kube.Close()
			config := &rest.Config{Host: kube.URL, BearerToken: "test-kube-only", TLSClientConfig: rest.TLSClientConfig{Insecure: true}}
			clientset, err := kubernetes.NewForConfig(config)
			require.NoError(t, err)
			client, cleanup := newMigrationHTTPClient(t.Context(), config, clientset, fixture.service())
			defer cleanup()
			req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "http://"+migrationTunnelHost+"/native", bytes.NewReader(payload))
			require.NoError(t, err)
			req.Header.Set("Authorization", "Bearer test-orka-only")
			req.Header.Set("Txn-Token", "test-transaction-only")
			response, err := client.Do(req)
			require.NoError(t, err)
			body, err := io.ReadAll(response.Body)
			require.NoError(t, err)
			require.NoError(t, response.Body.Close())
			require.Equal(t, payload, body)
			require.Eventually(t, func() bool { return fixture.closedCount() == 1 }, time.Second, time.Millisecond)
		})
	}
}

func migrationTestHTTPClient(t *testing.T, fixture *migrationKubeFixture) (*http.Client, func()) {
	t.Helper()
	kube := httptest.NewTLSServer(fixture)
	t.Cleanup(kube.Close)
	config := &rest.Config{Host: kube.URL, BearerToken: "test-kube-only", TLSClientConfig: rest.TLSClientConfig{Insecure: true}}
	clientset, err := kubernetes.NewForConfig(config)
	require.NoError(t, err)
	client, cleanup := newMigrationHTTPClient(t.Context(), config, clientset, fixture.service())
	t.Cleanup(cleanup)
	return client, cleanup
}

func TestMigrationTunnelCancellationAndCleanup(t *testing.T) {
	for _, websocket := range []bool{true, false} {
		for _, cancelRequest := range []bool{true, false} {
			name := fmt.Sprintf("websocket=%t/requestCancellation=%t", websocket, cancelRequest)
			t.Run(name, func(t *testing.T) {
				started, finished, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
				api := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
					defer close(finished)
					close(started)
					select {
					case <-r.Context().Done():
					case <-release:
					}
				}))
				defer api.Close()
				defer close(release)
				fixture := newMigrationKubeFixture(t, api.URL)
				fixture.websocket = websocket
				client, cleanup := migrationTestHTTPClient(t, fixture)
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				request, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+migrationTunnelHost+"/native", nil)
				require.NoError(t, err)
				result := make(chan error, 1)
				go func() {
					response, err := client.Do(request)
					if response != nil {
						_ = response.Body.Close()
					}
					result <- err
				}()
				select {
				case <-started:
				case <-time.After(3 * time.Second):
					t.Fatal("request did not reach backend")
				}
				if cancelRequest {
					cancel()
				} else {
					cleanup()
				}
				select {
				case err := <-result:
					require.Error(t, err)
				case <-time.After(3 * time.Second):
					t.Fatal("cancellation did not close request")
				}
				require.Eventually(t, func() bool { return fixture.closedCount() == 1 }, 3*time.Second, time.Millisecond)
				select {
				case <-finished:
				case <-time.After(3 * time.Second):
					t.Fatal("backend socket leaked")
				}
				cleanup() // Cleanup is idempotent.
			})
		}
	}
}

func TestMigrationTunnelForwardingFailureIsPrivate(t *testing.T) {
	fixture := newMigrationKubeFixture(t, "")
	fixture.failForward = true
	client, _ := migrationTestHTTPClient(t, fixture)
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+migrationTunnelHost+"/native", nil)
	require.NoError(t, err)
	response, err := client.Do(request)
	if response != nil {
		_ = response.Body.Close()
	}
	require.Error(t, err)
	require.NotContains(t, err.Error(), "private server diagnostic")
	require.Eventually(t, func() bool { return fixture.closedCount() == 1 }, time.Second, time.Millisecond)
}

func TestMigrationTunnelRejectsRedirectAndForeignDestination(t *testing.T) {
	attacker := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("private migration request escaped the tunnel") }))
	defer attacker.Close()
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, attacker.URL, http.StatusTemporaryRedirect)
	}))
	defer api.Close()
	fixture := newMigrationKubeFixture(t, api.URL)
	client, _ := migrationTestHTTPClient(t, fixture)
	// Kubernetes's localhost test server bypasses the ambient proxy; the
	// application host deliberately does not. Its transport must ignore it.
	t.Setenv("HTTP_PROXY", attacker.URL)
	t.Setenv("http_proxy", attacker.URL)
	request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "http://"+migrationTunnelHost+"/native", bytes.NewBufferString("private bundle"))
	require.NoError(t, err)
	request.Header.Set("Authorization", "Bearer test-orka-only")
	response, err := client.Do(request)
	require.NoError(t, err)
	require.Equal(t, http.StatusTemporaryRedirect, response.StatusCode)
	require.NoError(t, response.Body.Close())
	request, err = http.NewRequestWithContext(t.Context(), http.MethodPost, attacker.URL, bytes.NewBufferString("private bundle"))
	require.NoError(t, err)
	response, err = client.Do(request)
	if response != nil {
		_ = response.Body.Close()
	}
	require.ErrorContains(t, err, "invalid native migration tunnel destination")
	require.Equal(t, 1, fixture.tunnelCount())
}

func TestMigrationServiceBackendAdmission(t *testing.T) {
	for _, test := range []struct {
		name     string
		mutate   func(*corev1.Service, *corev1.Pod)
		wantPort int32
	}{
		{name: "named", wantPort: 9090},
		{name: "numeric", mutate: func(s *corev1.Service, _ *corev1.Pod) { s.Spec.Ports[0].TargetPort = intstr.FromInt32(9091) }, wantPort: 9091},
		{name: "recreated-service", mutate: func(s *corev1.Service, _ *corev1.Pod) { s.UID = "replacement" }},
		{name: "external-name", mutate: func(s *corev1.Service, _ *corev1.Pod) { s.Spec.Type = corev1.ServiceTypeExternalName }},
		{name: "no-selector", mutate: func(s *corev1.Service, _ *corev1.Pod) { s.Spec.Selector = nil }},
		{name: "wrong-service-port", mutate: func(s *corev1.Service, _ *corev1.Pod) { s.Spec.Ports[0].Port = 80 }},
		{name: "udp", mutate: func(s *corev1.Service, _ *corev1.Pod) { s.Spec.Ports[0].Protocol = corev1.ProtocolUDP }},
		{name: "missing-target-port", mutate: func(s *corev1.Service, _ *corev1.Pod) { s.Spec.Ports[0].TargetPort = intstr.FromString("missing") }},
		{name: "not-running", mutate: func(_ *corev1.Service, p *corev1.Pod) { p.Status.Phase = corev1.PodPending }},
		{name: "not-ready", mutate: func(_ *corev1.Service, p *corev1.Pod) { p.Status.Conditions = nil }},
		{name: "deleting", mutate: func(_ *corev1.Service, p *corev1.Pod) { now := metav1.Now(); p.DeletionTimestamp = &now }},
		{name: "wrong-labels", mutate: func(_ *corev1.Service, p *corev1.Pod) { p.Labels = map[string]string{"app": "other"} }},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newMigrationKubeFixture(t, "")
			service, pod := fixture.service(), migrationReadyPod()
			expected := service.DeepCopy()
			if test.mutate != nil {
				test.mutate(service, &pod)
			}
			kube := fake.NewClientset(service, &pod)
			name, port, err := migrationServicePod(t.Context(), kube, expected)
			if test.wantPort == 0 {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.Equal(t, pod.Name, name)
			require.Equal(t, test.wantPort, port)
		})
	}
}

func TestMigrationSPDYStalledUpgradeIsBounded(t *testing.T) {
	for _, failureBody := range []bool{false, true} {
		t.Run(fmt.Sprintf("failureBody=%t", failureBody), func(t *testing.T) {
			started, closed := make(chan struct{}), make(chan struct{})
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				require.Equal(t, http.MethodPost, r.Method)
				require.Equal(t, "Bearer test-kube-only", r.Header.Get("Authorization"))
				conn, buffer, err := w.(http.Hijacker).Hijack()
				require.NoError(t, err)
				defer func() { _ = conn.Close(); close(closed) }()
				if failureBody {
					_, err = buffer.WriteString("HTTP/1.1 400 Bad Request\r\nContent-Length: 100000\r\n\r\n")
					require.NoError(t, err)
					require.NoError(t, buffer.Flush())
				}
				close(started)
				// Never send headers, or never finish the failure body. A direct
				// socket read proves the client closed it rather than just returned.
				_, _ = io.Copy(io.Discard, conn)
			}))
			defer server.Close()
			config := &rest.Config{Host: server.URL, BearerToken: "test-kube-only", TLSClientConfig: rest.TLSClientConfig{Insecure: true}}
			ctx, cancel := context.WithTimeout(t.Context(), 500*time.Millisecond)
			defer cancel()
			result := make(chan error, 1)
			go func() {
				stream, err := dialMigrationSPDY(ctx, config, server.URL+"/portforward")
				if stream != nil {
					_ = stream.Close()
				}
				result <- err
			}()
			select {
			case <-started:
			case <-time.After(3 * time.Second):
				t.Fatal("upgrade did not reach server")
			}
			select {
			case err := <-result:
				require.Error(t, err)
			case <-time.After(3 * time.Second):
				t.Fatal("stalled upgrade outlived setup deadline")
			}
			select {
			case <-closed:
			case <-time.After(3 * time.Second):
				t.Fatal("stalled upgrade socket leaked")
			}
		})
	}
}

func TestMigrationUnacknowledgedStreamClosesSocket(t *testing.T) {
	for _, useWebsocket := range []bool{true, false} {
		for _, deadline := range []bool{true, false} {
			t.Run(fmt.Sprintf("websocket=%t/deadline=%t", useWebsocket, deadline), func(t *testing.T) {
				frameReceived, socketClosed := make(chan struct{}), make(chan struct{})
				fixture := newMigrationKubeFixture(t, "")
				server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path != "/api/v1/namespaces/test/pods/orka-backend/portforward" {
						fixture.ServeHTTP(w, r)
						return
					}
					require.Equal(t, "Bearer test-kube-only", r.Header.Get("Authorization"))
					if r.Method == http.MethodGet {
						if !useWebsocket {
							w.WriteHeader(http.StatusBadRequest)
							return
						}
						upgrader := gwebsocket.Upgrader{Subprotocols: []string{portprotocol.WebsocketsSPDYTunnelingPortForwardV1}}
						conn, err := upgrader.Upgrade(w, r, nil)
						require.NoError(t, err)
						defer func() { _ = conn.Close(); close(socketClosed) }()
						_, reader, err := conn.NextReader()
						require.NoError(t, err)
						_, _ = io.Copy(io.Discard, reader)
						close(frameReceived)
						// Consume frames without acknowledging any stream.
						for {
							_, reader, err := conn.NextReader()
							if err != nil {
								return
							}
							_, _ = io.Copy(io.Discard, reader)
						}
					}
					conn, buffer, err := w.(http.Hijacker).Hijack()
					require.NoError(t, err)
					defer func() { _ = conn.Close(); close(socketClosed) }()
					_, err = buffer.WriteString("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: SPDY/3.1\r\nX-Stream-Protocol-Version: portforward.k8s.io\r\n\r\n")
					require.NoError(t, err)
					require.NoError(t, buffer.Flush())
					var first [1]byte
					_, err = buffer.Read(first[:])
					require.NoError(t, err)
					close(frameReceived)
					_, _ = io.Copy(io.Discard, buffer)
				}))
				defer server.Close()
				config := &rest.Config{Host: server.URL, BearerToken: "test-kube-only", TLSClientConfig: rest.TLSClientConfig{Insecure: true}}
				clientset, err := kubernetes.NewForConfig(config)
				require.NoError(t, err)
				client, cleanup := newMigrationHTTPClient(t.Context(), config, clientset, fixture.service())
				defer cleanup()
				ctx, cancel := context.WithTimeout(t.Context(), 500*time.Millisecond)
				defer cancel()
				if !deadline {
					ctx = t.Context()
				}
				request, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+migrationTunnelHost+"/native", nil)
				require.NoError(t, err)
				result := make(chan error, 1)
				go func() {
					response, err := client.Do(request)
					if response != nil {
						_ = response.Body.Close()
					}
					result <- err
				}()
				select {
				case <-frameReceived:
				case <-time.After(3 * time.Second):
					t.Fatal("stream setup did not send a frame")
				}
				if !deadline {
					cleanup()
				}
				select {
				case err := <-result:
					require.Error(t, err)
				case <-time.After(3 * time.Second):
					t.Fatal("unacknowledged stream blocked command cleanup")
				}
				select {
				case <-socketClosed:
				case <-time.After(3 * time.Second):
					t.Fatal("unacknowledged stream retained its socket")
				}
				cleanup()
			})
		}
	}
}
