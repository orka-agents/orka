/*
Copyright (c) 2026.
MIT License - see LICENSE file for details.
*/
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/go-logr/logr/funcr"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	publisherservice "github.com/orka-agents/orka/internal/publisher/service"
)

// newTestManager builds a manager the way the controller does: per-object
// cache settings make construction resolve those types through discovery.
func newTestManager(t *testing.T, config *rest.Config) (ctrl.Manager, error) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	return ctrl.NewManager(config, ctrl.Options{
		Scheme:                 scheme,
		Logger:                 logr.Discard(),
		Metrics:                metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress: "0",
		Cache: cache.Options{ByObject: map[crclient.Object]cache.ByObject{
			&appsv1.Deployment{}: {},
			&corev1.Service{}:    {},
		}},
	})
}

func writeJSON(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, body)
}

// discoveryHandler serves just enough discovery for Deployments and Services.
func discoveryHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, `{"kind":"APIVersions","versions":["v1"],"serverAddressByClientCIDRs":[]}`)
	})
	mux.HandleFunc("/apis", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, `{"kind":"APIGroupList","apiVersion":"v1","groups":[{"name":"apps",`+
			`"versions":[{"groupVersion":"apps/v1","version":"v1"}],`+
			`"preferredVersion":{"groupVersion":"apps/v1","version":"v1"}}]}`)
	})
	mux.HandleFunc("/api/v1", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, `{"kind":"APIResourceList","groupVersion":"v1","resources":[`+
			`{"name":"services","singularName":"service","namespaced":true,"kind":"Service","verbs":["get","list","watch"]}]}`)
	})
	mux.HandleFunc("/apis/apps/v1", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, `{"kind":"APIResourceList","groupVersion":"apps/v1","resources":[`+
			`{"name":"deployments","singularName":"deployment","namespaced":true,"kind":"Deployment",`+
			`"verbs":["get","list","watch"]}]}`)
	})
	return mux
}

// statusHandler answers every request with one API status error.
func statusHandler(code int, reason string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, code, fmt.Sprintf(`{"kind":"Status","apiVersion":"v1","status":"Failure",`+
			`"message":%q,"reason":%q,"code":%d}`, strings.ToLower(reason), reason, code))
	})
}

// closedAddress returns a loopback address that refuses connections.
func closedAddress(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	return address
}

func failingDial(err error) func(context.Context, string, string) (net.Conn, error) {
	return func(_ context.Context, network, address string) (net.Conn, error) {
		return nil, &net.OpError{Op: "dial", Net: network, Addr: &net.TCPAddr{}, Err: err}
	}
}

// TestIsTransientConnectErrorClassifiesManagerFailures classifies
// the errors ctrl.NewManager actually returns, through controller-runtime's
// discovery and error wrapping, for each way the API server can fail.
func TestIsTransientConnectErrorClassifiesManagerFailures(t *testing.T) {
	api := httptest.NewServer(discoveryHandler())
	t.Cleanup(api.Close)
	tlsAPI := httptest.NewTLSServer(discoveryHandler())
	t.Cleanup(tlsAPI.Close)
	unauthorized := httptest.NewServer(statusHandler(http.StatusUnauthorized, "Unauthorized"))
	t.Cleanup(unauthorized.Close)
	forbidden := httptest.NewServer(statusHandler(http.StatusForbidden, "Forbidden"))
	t.Cleanup(forbidden.Close)

	tests := []struct {
		name      string
		config    *rest.Config
		wantErr   string
		is        func(error) bool
		transient bool
	}{
		{
			name:      "connection refused",
			config:    &rest.Config{Host: "http://" + closedAddress(t)},
			wantErr:   "connect: connection refused",
			transient: true,
		},
		{
			name: "dial timeout",
			config: &rest.Config{Host: api.URL, Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
				return (&net.Dialer{Timeout: time.Nanosecond}).DialContext(ctx, network, address)
			}},
			wantErr:   "i/o timeout",
			transient: true,
		},
		{
			name:      "no route to host",
			config:    &rest.Config{Host: api.URL, Dial: failingDial(os.NewSyscallError("connect", syscall.EHOSTUNREACH))},
			wantErr:   "connect: no route to host",
			transient: true,
		},
		{
			name:      "network unreachable",
			config:    &rest.Config{Host: api.URL, Dial: failingDial(os.NewSyscallError("connect", syscall.ENETUNREACH))},
			wantErr:   "connect: network is unreachable",
			transient: true,
		},
		{
			name: "temporary DNS failure",
			config: &rest.Config{Host: api.URL, Dial: failingDial(&net.DNSError{
				Err: "server misbehaving", Name: "kubernetes.default.svc", IsTemporary: true,
			})},
			wantErr:   "server misbehaving",
			transient: true,
		},
		{
			name: "unknown DNS name",
			config: &rest.Config{Host: api.URL, Dial: failingDial(&net.DNSError{
				Err: "no such host", Name: "kubernetes.example.invalid", IsNotFound: true,
			})},
			wantErr: "no such host",
		},
		{
			name:    "untrusted server certificate",
			config:  &rest.Config{Host: tlsAPI.URL},
			wantErr: "x509: certificate signed by unknown authority",
		},
		{
			name:    "unauthorized",
			config:  &rest.Config{Host: unauthorized.URL},
			wantErr: "the server has asked for the client to provide credentials",
			is:      apierrors.IsUnauthorized,
		},
		{
			name:   "forbidden",
			config: &rest.Config{Host: forbidden.URL},
			is:     apierrors.IsForbidden,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mgr, err := newTestManager(t, tt.config)
			if err == nil {
				t.Fatalf("NewManager succeeded with %T, want an error", mgr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) || (tt.is != nil && !tt.is(err)) {
				t.Fatalf("NewManager failed with %q, not the %s failure this case sets up", err, tt.name)
			}
			if got := isTransientConnectError(err); got != tt.transient {
				t.Fatalf("isTransientConnectError(%q) = %v, want %v", err, got, tt.transient)
			}
		})
	}

	if _, err := newTestManager(t, &rest.Config{Host: api.URL}); err != nil {
		t.Fatalf("NewManager against the reachable discovery server: %v", err)
	}
}

func TestIsTransientConnectErrorClassifiesWrappedErrors(t *testing.T) {
	// discovery wraps transport failures the way the restmapper reports them.
	wrap := func(err error) error {
		return fmt.Errorf("failed to determine if *v1.Service is namespaced: %w",
			fmt.Errorf("failed to get restmapping: %w", fmt.Errorf("failed to get server groups: %w", err)))
	}
	resource := schema.GroupResource{Group: "apps", Resource: "deployments"}
	tests := []struct {
		name      string
		err       error
		transient bool
	}{
		{name: "nil", err: nil},
		{name: "configuration", err: errors.New("must specify Config")},
		{
			// client-go already retries a reset GET several times before
			// returning it, so this case is not exercised through NewManager.
			name: "connection reset",
			err: wrap(&url.Error{Op: "Get", URL: "https://10.96.0.1:443/api", Err: &net.OpError{
				Op: "read", Net: "tcp", Err: os.NewSyscallError("read", syscall.ECONNRESET),
			}}),
			transient: true,
		},
		{name: "unauthorized", err: wrap(apierrors.NewUnauthorized("token expired"))},
		{name: "forbidden", err: wrap(apierrors.NewForbidden(resource, "", errors.New("denied")))},
		{name: "service unavailable", err: wrap(apierrors.NewServiceUnavailable("starting"))},
		{name: "server timeout", err: wrap(apierrors.NewTimeoutError("slow", 1))},
	}
	for _, tt := range tests {
		if got := isTransientConnectError(tt.err); got != tt.transient {
			t.Errorf("%s: isTransientConnectError(%v) = %v, want %v", tt.name, tt.err, got, tt.transient)
		}
	}
}

func TestRetryStartupStepCreatesManagerOnceAPIServerIsReachable(t *testing.T) {
	api := httptest.NewServer(discoveryHandler())
	t.Cleanup(api.Close)
	unrouted := closedAddress(t)

	// The first two attempts dial an address that refuses connections, standing
	// in for an API server that cannot be reached yet; later attempts reach it.
	const unreachableAttempts = 2
	var attempts atomic.Int32
	config := &rest.Config{Host: api.URL, Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
		if attempts.Load() <= unreachableAttempts {
			address = unrouted
		}
		return (&net.Dialer{}).DialContext(ctx, network, address)
	}}

	var mu sync.Mutex
	var logged []string
	log := funcr.New(func(_, args string) {
		mu.Lock()
		defer mu.Unlock()
		logged = append(logged, args)
	}, funcr.Options{})

	retry := testStartupRetry(log, 10*time.Second, time.Millisecond, 5*time.Millisecond)
	mgr, err := retryStartupStep(context.Background(), retry, "Kubernetes API server", isTransientConnectError,
		func() (ctrl.Manager, error) {
			attempts.Add(1)
			return newTestManager(t, config)
		})
	if err != nil {
		t.Fatalf("retryStartupStep: %v", err)
	}
	if mgr == nil {
		t.Fatal("retryStartupStep returned a nil manager")
	}
	if got := attempts.Load(); got != unreachableAttempts+1 {
		t.Fatalf("attempts = %d, want %d", got, unreachableAttempts+1)
	}
	mapping, err := mgr.GetRESTMapper().RESTMapping(schema.GroupKind{Group: "apps", Kind: "Deployment"}, "v1")
	if err != nil || mapping.Resource.Resource != "deployments" {
		t.Fatalf("RESTMapping(Deployment) = %v, %v; want deployments", mapping, err)
	}

	mu.Lock()
	defer mu.Unlock()
	var retries int
	for _, line := range logged {
		if strings.Contains(line, "not reachable yet; retrying") {
			retries++
			if !strings.Contains(line, `"Kubernetes API server"`) || !strings.Contains(line, `"attempt"`) ||
				!strings.Contains(line, "connection refused") {
				t.Errorf("retry log %s lacks the dependency, attempt or connection error", line)
			}
		}
	}
	if retries != unreachableAttempts {
		t.Fatalf("logged %d retries, want %d: %q", retries, unreachableAttempts, logged)
	}
}

func TestRetryStartupStepReturnsNonTransientErrorsImmediately(t *testing.T) {
	unauthorized := fmt.Errorf("failed to get restmapping: %w", apierrors.NewUnauthorized("token expired"))
	var calls int
	retry := testStartupRetry(logr.Discard(), time.Minute, time.Millisecond, time.Millisecond)
	_, err := retryStartupStep(context.Background(), retry, "Kubernetes API server", isTransientConnectError,
		func() (ctrl.Manager, error) {
			calls++
			return nil, unauthorized
		})
	if !errors.Is(err, unauthorized) {
		t.Fatalf("error = %v, want %v", err, unauthorized)
	}
	if calls != 1 {
		t.Fatalf("calls = %d, want 1", calls)
	}
}

func TestRetryStartupStepStopsAtTheDeadline(t *testing.T) {
	refused := &net.OpError{Op: "dial", Net: "tcp", Err: os.NewSyscallError("connect", syscall.ECONNREFUSED)}
	start := time.Now()
	retry := startupRetry{
		log: logr.Discard(), deadline: start.Add(100 * time.Millisecond),
		initialDelay: 10 * time.Millisecond, maxDelay: 20 * time.Millisecond,
	}
	var calls int
	_, err := retryStartupStep(context.Background(), retry, "Kubernetes API server", isTransientConnectError,
		func() (ctrl.Manager, error) {
			calls++
			return nil, refused
		})
	if !errors.Is(err, refused) {
		t.Fatalf("error = %v, want %v", err, refused)
	}
	if calls < 2 {
		t.Fatalf("calls = %d, want at least one retry", calls)
	}
	if elapsed := time.Since(start); elapsed > 100*time.Millisecond+time.Second {
		t.Fatalf("gave up after %s, want about the 100ms deadline", elapsed)
	}

	// The zero value, used where startup should not wait, allows one attempt.
	calls = 0
	if _, err := retryStartupStep(context.Background(), startupRetry{}, "Kubernetes API server", isTransientConnectError,
		func() (ctrl.Manager, error) {
			calls++
			return nil, refused
		}); !errors.Is(err, refused) || calls != 1 {
		t.Fatalf("zero startupRetry: error = %v after %d calls, want the refused error after 1", err, calls)
	}
}

func TestRetryStartupStepStopsWhenContextIsCancelled(t *testing.T) {
	refused := &net.OpError{Op: "dial", Net: "tcp", Err: os.NewSyscallError("connect", syscall.ECONNREFUSED)}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var calls int
	start := time.Now()
	retry := testStartupRetry(logr.Discard(), 2*time.Hour, time.Hour, time.Hour)
	_, err := retryStartupStep(ctx, retry, "Kubernetes API server", isTransientConnectError,
		func() (ctrl.Manager, error) {
			calls++
			time.AfterFunc(10*time.Millisecond, cancel)
			return nil, refused
		})
	if !errors.Is(err, refused) {
		t.Fatalf("error = %v, want %v", err, refused)
	}
	if calls != 1 {
		t.Fatalf("calls = %d, want 1", calls)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("returned after %s, want prompt return on cancellation", elapsed)
	}
}

func testStartupRetry(log logr.Logger, within, initialDelay, maxDelay time.Duration) startupRetry {
	return startupRetry{log: log, deadline: time.Now().Add(within), initialDelay: initialDelay, maxDelay: maxDelay}
}

// setPublisherEnv points workspacePublisherClientFromEnv at baseURL.
func setPublisherEnv(t *testing.T, baseURL string) {
	t.Helper()
	dir := t.TempDir()
	bearerPath := filepath.Join(dir, "controller-token")
	capabilityPath := filepath.Join(dir, "capability-secret")
	if err := os.WriteFile(bearerPath, []byte("controller-token-0123456789abcdef"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(capabilityPath, []byte("capability-secret-0123456789abcdef"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ORKA_WORKSPACE_PUBLISHER_URL", baseURL)
	t.Setenv("ORKA_WORKSPACE_PUBLISHER_CONTROLLER_TOKEN_FILE", bearerPath)
	t.Setenv("ORKA_WORKSPACE_PUBLISHER_CAPABILITY_SECRET_FILE", capabilityPath)
	t.Setenv("ORKA_ACP_ARTIFACT_CAPABILITY_SECRET_FILE", "")
}

func TestWorkspacePublisherClientFromEnvWaitsForPublisher(t *testing.T) {
	const wantLimit = int64(64 << 20)
	address := closedAddress(t)
	setPublisherEnv(t, "http://"+address)

	// The Publisher starts listening once the first attempt has been refused
	// and the retry logged, as a Publisher Pod restarting with the node does.
	var publisher *httptest.Server
	t.Cleanup(func() {
		if publisher != nil {
			publisher.Close()
		}
	})
	log := funcr.New(func(_, args string) {
		if publisher != nil || !strings.Contains(args, "not reachable yet; retrying") {
			return
		}
		if !strings.Contains(args, `"Workspace/Publisher"`) || !strings.Contains(args, "transport failed") {
			t.Errorf("retry log %s lacks the dependency or the error", args)
		}
		listener, err := net.Listen("tcp", address)
		if err != nil {
			t.Errorf("listen on %s: %v", address, err)
			return
		}
		publisher = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(publisherservice.CapabilitiesResponse{
				Protocol: publisherservice.ProtocolVersion,
				Limits:   publisherservice.CapabilityLimits{MaxWorkspaceArtifactBytes: wantLimit},
			})
		}))
		_ = publisher.Listener.Close()
		publisher.Listener = listener
		publisher.Start()
	}, funcr.Options{})

	retry := testStartupRetry(log, 10*time.Second, 10*time.Millisecond, 20*time.Millisecond)
	client, _, gotLimit, err := workspacePublisherClientFromEnv(context.Background(), retry)
	if err != nil {
		t.Fatalf("workspacePublisherClientFromEnv: %v", err)
	}
	if publisher == nil {
		t.Fatal("the first attempt succeeded; want it refused and retried")
	}
	if client == nil || gotLimit != wantLimit {
		t.Fatalf("client = %v, limit = %d; want a client and limit %d", client, gotLimit, wantLimit)
	}
}

func TestWorkspacePublisherClientFromEnvDoesNotRetryPublisherResponses(t *testing.T) {
	var requests atomic.Int32
	publisher := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"code":"unauthorized","message":"bad token"}`)
	}))
	t.Cleanup(publisher.Close)
	setPublisherEnv(t, publisher.URL)

	retry := testStartupRetry(logr.Discard(), time.Minute, time.Millisecond, time.Millisecond)
	if _, _, _, err := workspacePublisherClientFromEnv(context.Background(), retry); err == nil {
		t.Fatal("workspacePublisherClientFromEnv succeeded against a Publisher that rejects the controller")
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("Publisher requests = %d, want 1", got)
	}
}

// Toolbox flags are validated before startup waits for the API server; only
// the image-volume version check reads it, once it is reachable.
func TestACPToolboxPolicyValidatesFlagsWithoutAnAPIServer(t *testing.T) {
	if _, err := resolveACPToolboxPolicy(true, "", "", "", "bogus"); err == nil {
		t.Fatal("an invalid mount method was accepted")
	}
	if _, err := resolveACPToolboxPolicy(true, "", "", "not-a-label", "copy"); err == nil {
		t.Fatal("an invalid node selector was accepted")
	}
	copyPolicy, err := resolveACPToolboxPolicy(true, "registry.example", "", "", "copy")
	if err != nil {
		t.Fatal(err)
	}
	// A nil config would panic if the copy method contacted the API server.
	if got := checkACPToolboxImageVolumeSupport(nil, copyPolicy); got.UnavailableReason != "" {
		t.Fatalf("copy toolboxes marked unavailable: %s", got.UnavailableReason)
	}

	for gitVersion, wantUnavailable := range map[string]bool{"v1.35.2": true, "v1.36.0": false} {
		api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusOK, fmt.Sprintf(`{"gitVersion":%q}`, gitVersion))
		}))
		policy, err := resolveACPToolboxPolicy(true, "registry.example", "", "", "imageVolume")
		if err != nil {
			t.Fatal(err)
		}
		got := checkACPToolboxImageVolumeSupport(&rest.Config{Host: api.URL}, policy)
		api.Close()
		if (got.UnavailableReason != "") != wantUnavailable {
			t.Fatalf("API server %s: UnavailableReason = %q, want unavailable %v",
				gitVersion, got.UnavailableReason, wantUnavailable)
		}
	}
}
