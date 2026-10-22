// Package adapterkit serves an orka.storage.v1 provider as a separately
// deployed storage adapter.
//
// Serve wraps the wire server library with what an adapter process needs:
// TLS 1.3 with a serving certificate reloaded from disk, client
// authentication by bearer token, by mTLS client certificate or by both,
// per-class concurrency limits that shed load with OVERLOADED, liveness,
// readiness and metrics on a separate plain HTTP port, and a graceful stop.
// A provider uses SettleContext to finish a commit it has already sent after
// the caller's context ends.
//
// A minimal adapter:
//
//	func main() {
//		provider := openProvider() // implements storagev1.StateProvider
//		ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
//		defer stop()
//		err := adapterkit.Serve(ctx, adapterkit.Config{
//			State:          provider,
//			AdapterName:    "example-adapter",
//			AdapterVersion: version,
//			CertFile:       "/etc/orka-storage/tls/tls.crt",
//			KeyFile:        "/etc/orka-storage/tls/tls.key",
//			BearerTokenDir: "/etc/orka-storage/client-auth",
//		})
//		if err != nil {
//			log.Fatal(err)
//		}
//	}
package adapterkit

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	storagev1 "github.com/orka-agents/orka/pkg/storage/v1"
	"github.com/orka-agents/orka/pkg/storage/v1/wire"
)

// Defaults for unset Config fields.
const (
	DefaultAddress           = ":8443"
	DefaultProbeAddress      = ":8080"
	DefaultSettleTimeout     = 10 * time.Second
	DefaultReloadInterval    = 10 * time.Second
	DefaultReadinessInterval = 10 * time.Second
	DefaultShutdownTimeout   = 30 * time.Second
	// DefaultMaxConcurrentStreams caps the calls one client connection
	// carries at once.
	DefaultMaxConcurrentStreams = 256
)

// DefaultLimits returns the per-class limits Serve uses when Config.Limits
// leaves a class unset. They admit more calls than an Orka controller sends
// by default, so shedding happens only under abnormal load, and they keep
// health checks apart from storage traffic.
func DefaultLimits() wire.Limits {
	return wire.Limits{
		MaxConcurrent: map[wire.Class]int{wire.ClassHealth: 8, wire.ClassControl: 128, wire.ClassBulk: 16},
		MaxDuration: map[wire.Class]time.Duration{
			wire.ClassHealth: 10 * time.Second, wire.ClassControl: time.Minute, wire.ClassBulk: 5 * time.Minute,
		},
	}
}

// Config configures Serve.
type Config struct {
	// State is the state.v1 provider to serve, or nil.
	State storagev1.StateProvider
	// Artifacts is the artifacts.v1 provider to serve, or nil. At least one
	// of State and Artifacts is required; when both are set they must serve
	// the same dataset.
	Artifacts storagev1.ArtifactProvider
	// AdapterName and AdapterVersion identify the adapter in Describe.
	AdapterName    string
	AdapterVersion string

	// Address is the storage port's listen address. Empty means
	// DefaultAddress.
	Address string
	// Listener, when set, is the storage listener, and Address is ignored.
	// Serve closes it.
	Listener net.Listener
	// ProbeAddress serves /healthz, /readyz and /metrics over plain HTTP.
	// The probe port carries no stored data and needs no credentials, so
	// kubelet probes can reach it. Empty means DefaultProbeAddress.
	ProbeAddress string
	// ProbeListener, when set, is the probe listener, and ProbeAddress is
	// ignored. Serve closes it.
	ProbeListener net.Listener

	// CertFile and KeyFile hold the PEM serving certificate (with its chain)
	// and private key, such as the tls.crt and tls.key of a mounted
	// kubernetes.io/tls Secret. They are reloaded when they change.
	CertFile string
	KeyFile  string

	// BearerTokenDir, when set, enables bearer authentication: a call must
	// present "authorization: Bearer <token>" for a token stored in a
	// regular file of this directory whose name starts with "token", such
	// as the keys token and token-2 of a mounted Secret. Surrounding
	// whitespace in the files is ignored. The set is reloaded when the files
	// change, so a token is rotated by adding a second key, moving clients to
	// it and removing the first.
	BearerTokenDir string
	// ClientCAFile, when set, enables mTLS authentication: a client must
	// present a certificate that chains to a CA in this PEM bundle and
	// carries one of AllowedClientURIs or AllowedClientDNSNames as a subject
	// alternative name. The bundle is reloaded when it changes and may hold
	// an old and a new CA during rotation.
	ClientCAFile          string
	AllowedClientURIs     []string
	AllowedClientDNSNames []string
	// At least one of BearerTokenDir and ClientCAFile is required. With both,
	// a call must pass both.

	// Limits bounds concurrency and duration per call class. A class left
	// unset uses DefaultLimits.
	Limits wire.Limits
	// MaxBlobBytes caps the blob an upload may carry. Zero means
	// wire.DefaultMaxBlobBytes.
	MaxBlobBytes int64
	// MaxConcurrentStreams caps the calls one connection carries at once.
	// Zero means DefaultMaxConcurrentStreams.
	MaxConcurrentStreams uint32
	// SettleTimeout bounds the context SettleContext returns. Zero means
	// DefaultSettleTimeout.
	SettleTimeout time.Duration
	// ReloadInterval is how often credential files are checked for changes.
	// Zero means DefaultReloadInterval.
	ReloadInterval time.Duration
	// ReadinessInterval is how often the providers are described and
	// health-checked for readiness. Zero means DefaultReadinessInterval.
	ReadinessInterval time.Duration
	// ShutdownTimeout bounds how long a graceful stop waits for calls in
	// progress before it closes the remaining connections. Zero means
	// DefaultShutdownTimeout.
	ShutdownTimeout time.Duration
	// Logger receives the adapter's logs; it never receives request or
	// response content or credential values. Nil means slog.Default().
	Logger *slog.Logger
}

func (c *Config) applyDefaults() {
	if c.Address == "" {
		c.Address = DefaultAddress
	}
	if c.ProbeAddress == "" {
		c.ProbeAddress = DefaultProbeAddress
	}
	defaults := DefaultLimits()
	limits := wire.Limits{MaxConcurrent: map[wire.Class]int{}, MaxDuration: map[wire.Class]time.Duration{}}
	for _, class := range []wire.Class{wire.ClassHealth, wire.ClassControl, wire.ClassBulk} {
		limits.MaxConcurrent[class] = defaults.MaxConcurrent[class]
		if value := c.Limits.MaxConcurrent[class]; value > 0 {
			limits.MaxConcurrent[class] = value
		}
		limits.MaxDuration[class] = defaults.MaxDuration[class]
		if value := c.Limits.MaxDuration[class]; value > 0 {
			limits.MaxDuration[class] = value
		}
	}
	c.Limits = limits
	if c.MaxConcurrentStreams == 0 {
		c.MaxConcurrentStreams = DefaultMaxConcurrentStreams
	}
	if c.SettleTimeout <= 0 {
		c.SettleTimeout = DefaultSettleTimeout
	}
	if c.ReloadInterval <= 0 {
		c.ReloadInterval = DefaultReloadInterval
	}
	if c.ReadinessInterval <= 0 {
		c.ReadinessInterval = DefaultReadinessInterval
	}
	if c.ShutdownTimeout <= 0 {
		c.ShutdownTimeout = DefaultShutdownTimeout
	}
	if c.Logger == nil {
		c.Logger = slog.Default()
	}
}

func (c *Config) validate() error {
	var problems []error
	if c.State == nil && c.Artifacts == nil {
		problems = append(problems, errors.New("a state or artifacts provider is required"))
	}
	if c.CertFile == "" || c.KeyFile == "" {
		problems = append(problems, errors.New("a serving certificate and key are required: storage traffic uses TLS"))
	}
	if c.BearerTokenDir == "" && c.ClientCAFile == "" {
		problems = append(problems, errors.New("client authentication is required: set a bearer token directory, a client CA, or both"))
	}
	if c.ClientCAFile != "" && len(c.AllowedClientURIs) == 0 && len(c.AllowedClientDNSNames) == 0 {
		problems = append(problems, errors.New("mTLS authentication requires at least one allowed client URI or DNS name"))
	}
	if c.ClientCAFile == "" && (len(c.AllowedClientURIs) > 0 || len(c.AllowedClientDNSNames) > 0) {
		problems = append(problems, errors.New("allowed client names need a client CA"))
	}
	return errors.Join(problems...)
}

// Serve serves the configured providers until ctx ends, then stops
// gracefully: it reports not ready, lets calls in progress finish for up to
// ShutdownTimeout and closes the listeners. It returns nil after a stop that
// ctx requested, or the error that stopped serving. Invalid configuration
// or credentials fail before anything listens.
func Serve(ctx context.Context, config Config) error {
	// Serve owns the listeners it is given, including when it fails before
	// it serves.
	serving := false
	defer func() {
		if !serving {
			closeListeners(config.Listener, config.ProbeListener)
		}
	}()
	config.applyDefaults()
	if err := config.validate(); err != nil {
		return fmt.Errorf("storage adapter configuration: %w", err)
	}
	logger := config.Logger
	metrics := newMetrics()
	creds, err := loadCredentials(&config, metrics)
	if err != nil {
		return err
	}
	server, err := wire.NewServer(wire.ServerConfig{
		State:         config.State,
		Artifacts:     config.Artifacts,
		Info:          wire.ServerInfo{AdapterName: config.AdapterName, AdapterVersion: config.AdapterVersion},
		Authenticator: creds,
		Logger:        logger,
		Limits:        config.Limits,
		MaxBlobBytes:  config.MaxBlobBytes,
		Observer:      metrics,
	})
	if err != nil {
		return err
	}
	settle := settleInterceptors(config.SettleTimeout)
	grpcServer := grpc.NewServer(append(server.ServerOptions(),
		grpc.Creds(credentials.NewTLS(creds.serverTLS())),
		grpc.MaxConcurrentStreams(config.MaxConcurrentStreams),
		grpc.ChainUnaryInterceptor(settle.unary),
		grpc.ChainStreamInterceptor(settle.stream),
	)...)
	server.Register(grpcServer)

	listener, probeListener, err := listen(&config)
	if err != nil {
		return err
	}
	serving = true
	readiness := &readiness{config: &config, server: server, metrics: metrics, logger: logger}
	probe := &http.Server{
		Handler: probeHandler(readiness, metrics), ReadHeaderTimeout: 5 * time.Second, WriteTimeout: 10 * time.Second,
	}

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	errs := make(chan error, 2)
	var workers sync.WaitGroup
	workers.Go(func() {
		if err := grpcServer.Serve(listener); err != nil && !errors.Is(err, grpc.ErrServerStopped) {
			errs <- fmt.Errorf("storage server: %w", err)
		}
	})
	workers.Go(func() {
		if err := probe.Serve(probeListener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errs <- fmt.Errorf("probe server: %w", err)
		}
	})
	workers.Go(func() { creds.watch(runCtx, config.ReloadInterval) })
	workers.Go(func() { readiness.watch(runCtx) })
	logger.InfoContext(ctx, "storage adapter serving", "address", listener.Addr().String(),
		"probeAddress", probeListener.Addr().String(), "adapter", config.AdapterName)

	var serveErr error
	select {
	case <-ctx.Done():
	case serveErr = <-errs:
	}
	readiness.drain()
	cancel()
	stopGracefully(grpcServer, config.ShutdownTimeout)
	shutdownCtx, cancelShutdown := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancelShutdown()
	_ = probe.Shutdown(shutdownCtx)
	workers.Wait()
	logger.InfoContext(ctx, "storage adapter stopped")
	return serveErr
}

// listen opens the storage and probe listeners Serve has not been given.
func listen(config *Config) (net.Listener, net.Listener, error) {
	listener := config.Listener
	if listener == nil {
		var err error
		if listener, err = net.Listen("tcp", config.Address); err != nil {
			return nil, nil, fmt.Errorf("listen for storage calls: %w", err)
		}
	}
	probeListener := config.ProbeListener
	if probeListener == nil {
		var err error
		if probeListener, err = net.Listen("tcp", config.ProbeAddress); err != nil {
			if config.Listener == nil {
				_ = listener.Close()
			}
			return nil, nil, fmt.Errorf("listen for probes: %w", err)
		}
	}
	return listener, probeListener, nil
}

// closeListeners closes the listeners Serve was given.
func closeListeners(listeners ...net.Listener) {
	for _, listener := range listeners {
		if listener != nil {
			_ = listener.Close()
		}
	}
}

// stopGracefully lets calls in progress finish for up to timeout, then
// closes every connection.
func stopGracefully(server *grpc.Server, timeout time.Duration) {
	stopped := make(chan struct{})
	go func() {
		server.GracefulStop()
		close(stopped)
	}()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-stopped:
	case <-timer.C:
		server.Stop()
		<-stopped
	}
}

// readiness tracks whether the adapter can serve: its providers describe a
// dataset and pass their health checks, and it is not stopping.
type readiness struct {
	config  *Config
	server  *wire.Server
	metrics *metrics
	logger  *slog.Logger

	// mu orders a check's result against drain, so a check that finishes
	// after the adapter starts stopping never reports ready again.
	mu       sync.Mutex
	ready    bool
	draining bool
}

func (r *readiness) watch(ctx context.Context) {
	ticker := time.NewTicker(r.config.ReadinessInterval)
	defer ticker.Stop()
	for {
		r.check(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (r *readiness) check(ctx context.Context) {
	checkCtx, cancel := context.WithTimeout(ctx, r.config.Limits.MaxDuration[wire.ClassHealth])
	defer cancel()
	err := r.server.Refresh(checkCtx)
	if err == nil && r.config.State != nil {
		err = r.config.State.HealthCheck(checkCtx)
	}
	if err == nil && r.config.Artifacts != nil {
		err = r.config.Artifacts.HealthCheck(checkCtx)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	ready := err == nil && !r.draining
	if ready != r.ready {
		if ready {
			r.logger.InfoContext(ctx, "storage adapter ready", "dataset", r.server.DatasetID())
		} else if ctx.Err() == nil && !r.draining {
			r.logger.WarnContext(ctx, "storage adapter not ready", "error", err)
		}
	}
	r.ready = ready
	r.metrics.setReady(ready)
}

// isReady reports the last readiness check's result.
func (r *readiness) isReady() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.ready
}

// drain reports not ready for the rest of the process's life.
func (r *readiness) drain() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.draining = true
	r.ready = false
	r.metrics.setReady(false)
}

func probeHandler(readiness *readiness, metrics *metrics) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) {
		if !readiness.isReady() {
			http.Error(w, "not ready", http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("GET /metrics", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		_, _ = io.WriteString(w, metrics.exposition())
	})
	return mux
}
